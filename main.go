package main

import (
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/richardwilkes/toolbox/atexit"
	"github.com/richardwilkes/toolbox/cmdline"
	"github.com/richardwilkes/toolbox/errs"
	"github.com/richardwilkes/toolbox/log/jot"
	"github.com/richardwilkes/toolbox/taskqueue"
	"github.com/richardwilkes/toolbox/txt"
	"github.com/richardwilkes/toolbox/xio"
	"github.com/richardwilkes/toolbox/xio/fs"
	"github.com/yookoala/realpath"
	"golang.org/x/image/draw"
)

type status struct {
	total          int32
	converted      int32
	alreadyCorrect int32
	unsuitable     int32
	half           int32
	errors         int32
}

type options struct {
	outputRoot     string
	unsuitableRoot string
	inMultiple     int
	resizeMultiple int
	half           bool
}

func defaultOptions() *options {
	return &options{
		outputRoot:     "revised_images",
		unsuitableRoot: "unsuitable_images",
		inMultiple:     200,
		resizeMultiple: 140,
		half:           false,
	}
}

func (o *options) validate() {
	if o.outputRoot == "" {
		jot.Fatal(1, "output_root may not be empty")
	}
	if o.unsuitableRoot == "" {
		jot.Fatal(1, "unsuitable_root may not be empty")
	}
	if o.inMultiple < 1 {
		jot.Fatal(1, "must specify an in_multiple value greater than 0")
	}
	if o.resizeMultiple < 1 {
		jot.Fatal(1, "must specify an resize_multiple value greater than 0")
	}
	if o.half {
		if o.inMultiple%2 == 1 {
			jot.Fatal(1, "must specify an even value for in_multiple when half is set")
		}
		if o.resizeMultiple%2 == 1 {
			jot.Fatal(1, "must specify an even value for resize_multiple when half is set")
		}
	}
}

func main() {
	cmdline.AppVersion = "0.1"
	cmdline.CopyrightStartYear = "2019"
	cmdline.CopyrightHolder = "Richard A. Wilkes"
	cmdline.AppIdentifier = "com.trollworks.scaleimg"
	cl := cmdline.New(true)
	opts := defaultOptions()
	cl.NewGeneralOption(&opts.outputRoot).SetSingle('o').SetName("output_root").SetUsage("Location to store the converted images")
	cl.NewGeneralOption(&opts.unsuitableRoot).SetSingle('u').SetName("unsuitable_root").SetUsage("Location to store the images that were unsuitable for conversion")
	cl.NewGeneralOption(&opts.inMultiple).SetSingle('i').SetName("in_multiple").SetUsage("Only process image files whose dimensions are exact multiples of this value")
	cl.NewGeneralOption(&opts.resizeMultiple).SetSingle('r').SetName("resize_multiple").SetUsage("Resize images to a multiple of this value")
	cl.NewGeneralOption(&opts.half).SetSingle('2').SetName("half").SetUsage("Also process images files whose width or height is half of an exact multiple of the in_multiple value")
	paths := cl.Parse(os.Args[1:])
	opts.validate()

	// If no paths specified, use the current directory
	if len(paths) == 0 {
		wd, err := os.Getwd()
		jot.FatalIfErr(err)
		paths = append(paths, wd)
	}

	// Determine the actual root paths and prune out paths that are a subset
	// of another
	set := make(map[string]bool)
	for _, path := range paths {
		actual, err := realpath.Realpath(path)
		jot.FatalIfErr(err)
		if _, exists := set[actual]; !exists {
			add := true
			for one := range set {
				prefixed := strings.HasPrefix(rel(one, actual), "..")
				if prefixed != strings.HasPrefix(rel(actual, one), "..") {
					if prefixed {
						delete(set, one)
					} else {
						add = false
						break
					}
				}
			}
			if add {
				set[actual] = true
			}
		}
	}

	// Collect the files
	var list []string
	for path := range set {
		jot.FatalIfErr(filepath.Walk(path, func(p string, info os.FileInfo, _ error) error {
			// Prune out hidden directories and files
			name := info.Name()
			if strings.HasPrefix(name, ".") {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// If this is a gif, jpg or png file, add it to the list
			if !info.IsDir() {
				lower := strings.ToLower(p)
				if strings.HasSuffix(lower, ".gif") ||
					strings.HasSuffix(lower, ".jpg") ||
					strings.HasSuffix(lower, ".jpeg") ||
					strings.HasSuffix(lower, ".png") {
					list = append(list, p)
				}
			}
			return nil
		}))
	}
	sort.Slice(list, func(i, j int) bool { return txt.NaturalLess(list[i], list[j], true) })

	// Process the files
	tq := taskqueue.New(taskqueue.RecoveryHandler(func(rErr error) { jot.Error(rErr) }))
	var s status
	for _, path := range list {
		tq.Submit(newTask(path, opts, &s))
	}
	tq.Shutdown()
	jot.Flush()
	width := len(fmt.Sprintf("%d", s.total))
	fmt.Printf(fmt.Sprintf("%%%dd images examined\n", width), s.total)
	fmt.Printf(fmt.Sprintf("%%%dd images converted\n", width), s.converted)
	if s.alreadyCorrect > 0 {
		fmt.Printf(fmt.Sprintf("%%%dd images already correct\n", width), s.alreadyCorrect)
	}
	if s.unsuitable > 0 {
		fmt.Printf(fmt.Sprintf("%%%dd images unsuitable\n", width), s.unsuitable)
	}
	if s.half > 0 {
		fmt.Printf(fmt.Sprintf("%%%dd images half suitable\n", width), s.half)
	}
	if s.errors > 0 {
		fmt.Printf(fmt.Sprintf("%%%dd errors\n", width), s.errors)
	}
	atexit.Exit(0)
}

func rel(base, target string) string {
	path, err := filepath.Rel(base, target)
	jot.FatalIfErr(err)
	return path
}

func newTask(path string, opts *options, s *status) taskqueue.Task {
	return func() {
		processFile(path, opts, s)
	}
}

func processFile(path string, opts *options, s *status) {
	atomic.AddInt32(&s.total, 1)
	img, err := loadImage(path)
	if err != nil {
		atomic.AddInt32(&s.errors, 1)
		jot.Error(err)
		return
	}
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	switch {
	case width%opts.inMultiple == 0 && height%opts.inMultiple == 0:
		dstBounds := image.Rect(0, 0, (width/opts.inMultiple)*opts.resizeMultiple, (height/opts.inMultiple)*opts.resizeMultiple)
		dst := image.NewRGBA(dstBounds)
		draw.CatmullRom.Scale(dst, dstBounds, img, bounds, draw.Over, nil)
		if err = writePNG(opts, path, dst); err != nil {
			atomic.AddInt32(&s.errors, 1)
			jot.Error(errs.Wrap(err))
			return
		}
		atomic.AddInt32(&s.converted, 1)
	case width%opts.resizeMultiple == 0 && height%opts.resizeMultiple == 0:
		if err = fs.Copy(path, transformPathForImage(opts, path, img)); err != nil {
			atomic.AddInt32(&s.errors, 1)
			jot.Error(errs.Wrap(err))
			return
		}
		atomic.AddInt32(&s.alreadyCorrect, 1)
	case opts.half && (width%opts.inMultiple == 0 || width%(opts.inMultiple/2) == 0) && (height%opts.inMultiple == 0 || height%(opts.inMultiple/2) == 0):
		dstBounds := image.Rect(0, 0, ((width*2)/opts.inMultiple)*(opts.resizeMultiple/2), ((height*2)/opts.inMultiple)*(opts.resizeMultiple/2))
		dst := image.NewRGBA(dstBounds)
		draw.CatmullRom.Scale(dst, dstBounds, img, bounds, draw.Over, nil)
		if err = writePNG(opts, path, dst); err != nil {
			atomic.AddInt32(&s.errors, 1)
			jot.Error(errs.Wrap(err))
			return
		}
		atomic.AddInt32(&s.half, 1)
	default:
		if err = fs.Copy(path, filepath.Join(opts.unsuitableRoot, path)); err != nil {
			atomic.AddInt32(&s.errors, 1)
			jot.Error(errs.Wrap(err))
			return
		}
		atomic.AddInt32(&s.unsuitable, 1)
	}
}

func loadImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	defer xio.CloseIgnoringErrors(f)
	img, _, err := image.Decode(f)
	return img, errs.Wrap(err)
}

func writePNG(opts *options, path string, img image.Image) error {
	p := transformPathForImage(opts, path, img)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return errs.Wrap(err)
	}
	f, err := os.Create(p)
	if err != nil {
		return errs.Wrap(err)
	}
	if err = png.Encode(f, img); err != nil {
		xio.CloseIgnoringErrors(f)
		os.Remove(p) //nolint:errcheck // Don't care
		return errs.Wrap(err)
	}
	if err = f.Close(); err != nil {
		os.Remove(p) //nolint:errcheck // Don't care
		return errs.Wrap(err)
	}
	return nil
}

var dimensionsRegexp = regexp.MustCompile(`\d+[xX]\d+`)

func transformPathForImage(opts *options, path string, img image.Image) string {
	bounds := img.Bounds()
	width := bounds.Dx()
	widthStr := fmt.Sprintf("%d", width/opts.resizeMultiple)
	if width%opts.resizeMultiple != 0 {
		if width/opts.resizeMultiple == 0 {
			widthStr = "½"
		} else {
			widthStr += "½"
		}
	}
	height := bounds.Dy()
	heightStr := fmt.Sprintf("%d", height/opts.resizeMultiple)
	if height%opts.resizeMultiple != 0 {
		if height/opts.resizeMultiple == 0 {
			heightStr = "½"
		} else {
			heightStr += "½"
		}
	}
	path = strings.ReplaceAll(fs.TrimExtension(path), "_", " ")
	dir := filepath.Dir(path)
	base := txt.CollapseSpaces(strings.TrimSpace(dimensionsRegexp.ReplaceAllLiteralString(filepath.Base(path), "")))
	return fmt.Sprintf("%s - %sx%s.png", filepath.Join(opts.outputRoot, dir, base), widthStr, heightStr)
}
