/*
Copyright (c) 2012, Jan Schlicht <jan.schlicht@gmail.com>

Permission to use, copy, modify, and/or distribute this software for any purpose
with or without fee is hereby granted, provided that the above copyright notice
and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY AND
FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM LOSS
OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER
TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF
THIS SOFTWARE.
*/

// Package resize implements various image resizing methods.
//
// The package works with the Image interface described in the image package.
// Various interpolation methods are provided and multiple processors may be
// utilized in the computations.
//
// Example:
//     imgResized := resize.Resize(1000, 0, imgOld, resize.MitchellNetravali)
package resize

import (
	"image"
	"runtime"
	"strings"
	"sync"
)

// An InterpolationFunction provides the parameters that describe an
// interpolation kernel. It returns the number of samples to take
// and the kernel function to use for sampling.
type InterpolationFunction int

// InterpolationFunction constants
const (
	// Nearest-neighbor interpolation
	NearestNeighbor InterpolationFunction = iota
	// Bilinear interpolation
	Bilinear
	// Bicubic interpolation (with cubic hermite spline)
	Bicubic
	// Mitchell-Netravali interpolation
	MitchellNetravali
	// Lanczos interpolation (a=2)
	Lanczos2
	// Lanczos interpolation (a=3)
	Lanczos3
)

// kernal, returns an InterpolationFunctions taps and kernel.
func (i InterpolationFunction) kernel() (int, func(float64) float64) {
	switch i {
	case Bilinear:
		return 2, linear
	case Bicubic:
		return 4, cubic
	case MitchellNetravali:
		return 4, mitchellnetravali
	case Lanczos2:
		return 4, lanczos2
	case Lanczos3:
		return 6, lanczos3
	default:
		// Default to NearestNeighbor.
		return 2, nearest
	}
}

// values <1 will sharpen the image
var blur = 1.0

// Resize scales an image to new width and height using the interpolation function interp.
// A new image with the given dimensions will be returned.
// If one of the parameters width or height is set to 0, its size will be calculated so that
// the aspect ratio is that of the originating image.
// The resizing algorithm uses channels for parallel computation.
func Resize(width, height uint, img image.Image, interp InterpolationFunction) (image.Image, error) {
	scaleX, scaleY := calcFactors(width, height, float64(img.Bounds().Dx()), float64(img.Bounds().Dy()))
	if width == 0 {
		width = uint(0.7 + float64(img.Bounds().Dx())/scaleX)
	}
	if height == 0 {
		height = uint(0.7 + float64(img.Bounds().Dy())/scaleY)
	}

	// Trivial case: return input image
	if int(width) == img.Bounds().Dx() && int(height) == img.Bounds().Dy() {
		return img, nil
	}

	if interp == NearestNeighbor {
		return resizeNearest(width, height, scaleX, scaleY, img, interp)
	}

	taps, kernel := interp.kernel()
	cpus := runtime.NumCPU()
	wg := sync.WaitGroup{}
	var panics chan string

	// Generic access to image.Image is slow in tight loops.
	// The optimal access has to be determined from the concrete image type.
	switch input := img.(type) {
	case *image.RGBA:
		// 8-bit precision
		temp := image.NewRGBA(image.Rect(0, 0, input.Bounds().Dy(), int(width)))
		result := image.NewRGBA(image.Rect(0, 0, int(width), int(height)))

		// horizontal filter, results in transposed temporary image
		coeffs, offset, filterLength := createWeights8(temp.Bounds().Dy(), taps, blur, scaleX, kernel)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*image.RGBA)
			go func() {
				defer recoverfn(&wg, panics)
				resizeRGBA(input, slice, scaleX, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		// horizontal filter on transposed image, result is not transposed
		coeffs, offset, filterLength = createWeights8(result.Bounds().Dy(), taps, blur, scaleY, kernel)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*image.RGBA)
			go func() {
				defer recoverfn(&wg, panics)
				resizeRGBA(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result, nil
	case *image.YCbCr:
		// 8-bit precision
		// accessing the YCbCr arrays in a tight loop is slow.
		// converting the image to ycc increases performance by 2x.
		temp := newYCC(image.Rect(0, 0, input.Bounds().Dy(), int(width)), input.SubsampleRatio)
		result := newYCC(image.Rect(0, 0, int(width), int(height)), input.SubsampleRatio)

		coeffs, offset, filterLength := createWeights8(temp.Bounds().Dy(), taps, blur, scaleX, kernel)
		in := imageYCbCrToYCC(input)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*ycc)
			go func() {
				defer recoverfn(&wg, panics)
				resizeYCbCr(in, slice, scaleX, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		coeffs, offset, filterLength = createWeights8(result.Bounds().Dy(), taps, blur, scaleY, kernel)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*ycc)
			go func() {
				defer recoverfn(&wg, panics)
				resizeYCbCr(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result.YCbCr(), nil
	case *image.RGBA64:
		// 16-bit precision
		temp := image.NewRGBA64(image.Rect(0, 0, input.Bounds().Dy(), int(width)))
		result := image.NewRGBA64(image.Rect(0, 0, int(width), int(height)))

		// horizontal filter, results in transposed temporary image
		coeffs, offset, filterLength := createWeights16(temp.Bounds().Dy(), taps, blur, scaleX, kernel)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*image.RGBA64)
			go func() {
				defer recoverfn(&wg, panics)
				resizeRGBA64(input, slice, scaleX, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		// horizontal filter on transposed image, result is not transposed
		coeffs, offset, filterLength = createWeights16(result.Bounds().Dy(), taps, blur, scaleY, kernel)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*image.RGBA64)
			go func() {
				defer recoverfn(&wg, panics)
				resizeGeneric(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result, nil
	case *image.Gray:
		// 8-bit precision
		temp := image.NewGray(image.Rect(0, 0, input.Bounds().Dy(), int(width)))
		result := image.NewGray(image.Rect(0, 0, int(width), int(height)))

		// horizontal filter, results in transposed temporary image
		coeffs, offset, filterLength := createWeights8(temp.Bounds().Dy(), taps, blur, scaleX, kernel)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*image.Gray)
			go func() {
				defer recoverfn(&wg, panics)
				resizeGray(input, slice, scaleX, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		// horizontal filter on transposed image, result is not transposed
		coeffs, offset, filterLength = createWeights8(result.Bounds().Dy(), taps, blur, scaleY, kernel)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*image.Gray)
			go func() {
				defer recoverfn(&wg, panics)
				resizeGray(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result, nil
	case *image.Gray16:
		// 16-bit precision
		temp := image.NewGray16(image.Rect(0, 0, input.Bounds().Dy(), int(width)))
		result := image.NewGray16(image.Rect(0, 0, int(width), int(height)))

		// horizontal filter, results in transposed temporary image
		coeffs, offset, filterLength := createWeights16(temp.Bounds().Dy(), taps, blur, scaleX, kernel)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*image.Gray16)
			go func() {
				defer recoverfn(&wg, panics)
				resizeGray16(input, slice, scaleX, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		// horizontal filter on transposed image, result is not transposed
		coeffs, offset, filterLength = createWeights16(result.Bounds().Dy(), taps, blur, scaleY, kernel)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*image.Gray16)
			go func() {
				defer recoverfn(&wg, panics)
				resizeGray16(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result, nil
	default:
		// 16-bit precision
		temp := image.NewRGBA64(image.Rect(0, 0, img.Bounds().Dy(), int(width)))
		result := image.NewRGBA64(image.Rect(0, 0, int(width), int(height)))

		// horizontal filter, results in transposed temporary image
		coeffs, offset, filterLength := createWeights16(temp.Bounds().Dy(), taps, blur, scaleX, kernel)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*image.RGBA64)
			go func() {
				defer recoverfn(&wg, panics)
				resizeGeneric(img, slice, scaleX, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		// horizontal filter on transposed image, result is not transposed
		coeffs, offset, filterLength = createWeights16(result.Bounds().Dy(), taps, blur, scaleY, kernel)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*image.RGBA64)
			go func() {
				defer recoverfn(&wg, panics)
				resizeRGBA64(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result, nil
	}
}

func resizeNearest(width, height uint, scaleX, scaleY float64, img image.Image, interp InterpolationFunction) (image.Image, error) {
	taps, _ := interp.kernel()
	cpus := runtime.NumCPU()
	wg := sync.WaitGroup{}
	var panics chan string

	switch input := img.(type) {
	case *image.RGBA:
		// 8-bit precision
		temp := image.NewRGBA(image.Rect(0, 0, input.Bounds().Dy(), int(width)))
		result := image.NewRGBA(image.Rect(0, 0, int(width), int(height)))

		// horizontal filter, results in transposed temporary image
		coeffs, offset, filterLength := createWeightsNearest(temp.Bounds().Dy(), taps, blur, scaleX)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*image.RGBA)
			go func() {
				defer recoverfn(&wg, panics)
				nearestRGBA(input, slice, scaleX, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		// horizontal filter on transposed image, result is not transposed
		coeffs, offset, filterLength = createWeightsNearest(result.Bounds().Dy(), taps, blur, scaleY)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*image.RGBA)
			go func() {
				defer recoverfn(&wg, panics)
				nearestRGBA(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result, nil
	case *image.YCbCr:
		// 8-bit precision
		// accessing the YCbCr arrays in a tight loop is slow.
		// converting the image to ycc increases performance by 2x.
		temp := newYCC(image.Rect(0, 0, input.Bounds().Dy(), int(width)), input.SubsampleRatio)
		result := newYCC(image.Rect(0, 0, int(width), int(height)), input.SubsampleRatio)

		coeffs, offset, filterLength := createWeightsNearest(temp.Bounds().Dy(), taps, blur, scaleX)
		in := imageYCbCrToYCC(input)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*ycc)
			go func() {
				defer recoverfn(&wg, panics)
				nearestYCbCr(in, slice, scaleX, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		coeffs, offset, filterLength = createWeightsNearest(result.Bounds().Dy(), taps, blur, scaleY)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*ycc)
			go func() {
				defer recoverfn(&wg, panics)
				nearestYCbCr(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result.YCbCr(), nil
	case *image.RGBA64:
		// 16-bit precision
		temp := image.NewRGBA64(image.Rect(0, 0, input.Bounds().Dy(), int(width)))
		result := image.NewRGBA64(image.Rect(0, 0, int(width), int(height)))

		// horizontal filter, results in transposed temporary image
		coeffs, offset, filterLength := createWeightsNearest(temp.Bounds().Dy(), taps, blur, scaleX)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*image.RGBA64)
			go func() {
				defer recoverfn(&wg, panics)
				nearestRGBA64(input, slice, scaleX, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		// horizontal filter on transposed image, result is not transposed
		coeffs, offset, filterLength = createWeightsNearest(result.Bounds().Dy(), taps, blur, scaleY)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*image.RGBA64)
			go func() {
				defer recoverfn(&wg, panics)
				nearestGeneric(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result, nil
	case *image.Gray:
		// 8-bit precision
		temp := image.NewGray(image.Rect(0, 0, input.Bounds().Dy(), int(width)))
		result := image.NewGray(image.Rect(0, 0, int(width), int(height)))

		// horizontal filter, results in transposed temporary image
		coeffs, offset, filterLength := createWeightsNearest(temp.Bounds().Dy(), taps, blur, scaleX)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*image.Gray)
			go func() {
				defer recoverfn(&wg, panics)
				nearestGray(input, slice, scaleX, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		// horizontal filter on transposed image, result is not transposed
		coeffs, offset, filterLength = createWeightsNearest(result.Bounds().Dy(), taps, blur, scaleY)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*image.Gray)
			go func() {
				defer recoverfn(&wg, panics)
				nearestGray(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result, nil
	case *image.Gray16:
		// 16-bit precision
		temp := image.NewGray16(image.Rect(0, 0, input.Bounds().Dy(), int(width)))
		result := image.NewGray16(image.Rect(0, 0, int(width), int(height)))

		// horizontal filter, results in transposed temporary image
		coeffs, offset, filterLength := createWeightsNearest(temp.Bounds().Dy(), taps, blur, scaleX)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*image.Gray16)
			go func() {
				defer recoverfn(&wg, panics)
				nearestGray16(input, slice, scaleX, coeffs, offset, filterLength)
			}()
		}

		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		// horizontal filter on transposed image, result is not transposed
		coeffs, offset, filterLength = createWeightsNearest(result.Bounds().Dy(), taps, blur, scaleY)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*image.Gray16)
			go func() {
				defer recoverfn(&wg, panics)
				nearestGray16(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result, nil
	default:
		// 16-bit precision
		temp := image.NewRGBA64(image.Rect(0, 0, img.Bounds().Dy(), int(width)))
		result := image.NewRGBA64(image.Rect(0, 0, int(width), int(height)))

		// horizontal filter, results in transposed temporary image
		coeffs, offset, filterLength := createWeightsNearest(temp.Bounds().Dy(), taps, blur, scaleX)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(temp, i, cpus).(*image.RGBA64)
			go func() {
				defer recoverfn(&wg, panics)
				nearestGeneric(img, slice, scaleX, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		// horizontal filter on transposed image, result is not transposed
		coeffs, offset, filterLength = createWeightsNearest(result.Bounds().Dy(), taps, blur, scaleY)
		wg.Add(cpus)
		panics = makePanicChan(cpus)
		for i := 0; i < cpus; i++ {
			slice := makeSlice(result, i, cpus).(*image.RGBA64)
			go func() {
				defer recoverfn(&wg, panics)
				nearestRGBA64(temp, slice, scaleY, coeffs, offset, filterLength)
			}()
		}
		wg.Wait()
		if err := retrieveErrors(panics); err != nil {
			return nil, err
		}

		return result, nil
	}

}

// Calculates scaling factors using old and new image dimensions.
func calcFactors(width, height uint, oldWidth, oldHeight float64) (scaleX, scaleY float64) {
	if width == 0 {
		if height == 0 {
			scaleX = 1.0
			scaleY = 1.0
		} else {
			scaleY = oldHeight / float64(height)
			scaleX = scaleY
		}
	} else {
		scaleX = oldWidth / float64(width)
		if height == 0 {
			scaleY = scaleX
		} else {
			scaleY = oldHeight / float64(height)
		}
	}
	return
}

type imageWithSubImage interface {
	image.Image
	SubImage(image.Rectangle) image.Image
}

func makeSlice(img imageWithSubImage, i, n int) image.Image {
	return img.SubImage(image.Rect(img.Bounds().Min.X, img.Bounds().Min.Y+i*img.Bounds().Dy()/n, img.Bounds().Max.X, img.Bounds().Min.Y+(i+1)*img.Bounds().Dy()/n))
}

func recoverfn(wg *sync.WaitGroup, panics chan string) {
	defer wg.Done()

	if rc := recover(); rc != nil {
		str, ok := rc.(string)
		if ok {
			panics <- str
		} else {
			panics <- "Unknown panic"
		}
	}
}

func makePanicChan(size int) chan string {
	return make(chan string, size)
}

type resizeErrors struct {
	errs []string
}

func newResizeErrors(max int) *resizeErrors {
	r := resizeErrors{}
	r.errs = make([]string, 0, max)
	return &r
}

func (r *resizeErrors) Error() string {
	return strings.Join(r.errs, ", ")
}

func retrieveErrors(panics chan string) error {
	var e *resizeErrors
	close(panics)
	max := cap(panics)

	for str := range panics {
		if e == nil {
			e = newResizeErrors(max)
		}

		e.errs = append(e.errs, str)
	}

	if e == nil {
		return nil
	} else {
		return e
	}
}
