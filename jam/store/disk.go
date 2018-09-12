package store

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/gobuffalo/packr/encoding/hex"

	"github.com/gobuffalo/packr/jam/parser"
	"github.com/karrick/godirwalk"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var _ Store = &Disk{}

type Disk struct {
	DBPath    string
	DBPackage string
	global    map[string]string
	boxes     map[string]*parser.Box
	moot      *sync.RWMutex
}

func NewDisk(path string, pkg string) *Disk {
	if len(path) == 0 {
		path = "packrd"
	}
	if len(pkg) == 0 {
		pkg = "packrd"
	}
	if !filepath.IsAbs(path) {
		path, _ = filepath.Abs(path)
	}
	return &Disk{
		DBPath:    path,
		DBPackage: pkg,
		global:    map[string]string{},
		boxes:     map[string]*parser.Box{},
		moot:      &sync.RWMutex{},
	}
}

func (d *Disk) FileNames(box *parser.Box) ([]string, error) {
	path := box.AbsPath
	if len(box.AbsPath) == 0 {
		path = box.Path
	}
	var names []string
	err := godirwalk.Walk(path, &godirwalk.Options{
		FollowSymbolicLinks: true,
		Callback: func(path string, de *godirwalk.Dirent) error {
			if !de.IsRegular() {
				return nil
			}
			names = append(names, path)
			return nil
		},
	})
	return names, err
}

func (d *Disk) Files(box *parser.Box) ([]*parser.File, error) {
	var files []*parser.File
	names, err := d.FileNames(box)
	if err != nil {
		return files, errors.WithStack(err)
	}
	for _, n := range names {
		b, err := ioutil.ReadFile(n)
		if err != nil {
			return files, errors.WithStack(err)
		}
		f := parser.NewFile(n, bytes.NewReader(b))
		files = append(files, f)
	}
	return files, nil
}

func (d *Disk) Pack(box *parser.Box) error {
	d.boxes[box.Name] = box
	names, err := d.FileNames(box)
	if err != nil {
		return errors.WithStack(err)
	}
	for _, n := range names {
		k, ok := d.global[n]
		if ok {
			continue
		}
		k = makeKey(box, n)
		// not in the global, so add it!
		d.global[n] = k
	}
	return nil
}

func (d *Disk) Clean(box *parser.Box) error {
	root := box.PackageDir
	if len(root) == 0 {
		return errors.New("can't clean an empty box.PackageDir")
	}
	return Clean(root)
}

type options struct {
	Package     string
	GlobalFiles map[string]string
	Boxes       []optsBox
}

type optsBox struct {
	Name string
	Path string
}

func (d *Disk) Close() error {
	opts := options{
		Package:     d.DBPackage,
		GlobalFiles: map[string]string{},
	}
	wg := errgroup.Group{}
	for k, v := range d.global {
		func(k, v string) {
			wg.Go(func() error {
				fmt.Println("encoding", k, v)
				bb := &bytes.Buffer{}
				enc := hex.NewEncoder(bb)
				zw := gzip.NewWriter(enc)
				f, err := os.Open(k)
				if err != nil {
					return errors.WithStack(err)
				}
				defer f.Close()
				io.Copy(zw, f)
				if err := zw.Close(); err != nil {
					return errors.WithStack(err)
				}
				d.moot.Lock()
				opts.GlobalFiles[v] = bb.String()
				d.moot.Unlock()
				return nil
			})
		}(k, v)
	}

	if err := wg.Wait(); err != nil {
		return errors.WithStack(err)
	}
	for _, b := range d.boxes {
		ob := optsBox{
			Name: b.Name,
		}
		opts.Boxes = append(opts.Boxes, ob)
	}
	sort.Slice(opts.Boxes, func(a, b int) bool {
		return opts.Boxes[a].Name < opts.Boxes[b].Name
	})

	t := template.New("").Funcs(template.FuncMap{
		"printFiles": func(ob optsBox) (template.HTML, error) {
			box := d.boxes[ob.Name]
			if box == nil {
				return "", errors.Errorf("could not find box %s", ob.Name)
			}
			bb := &bytes.Buffer{}
			fn, err := d.FileNames(box)
			if err != nil {
				return "", errors.WithStack(err)
			}
			const ln = "\t\tb.SetResolver(\"%s\", packr.Pointer{ForwardBox: gk, ForwardPath: \"%s\"})\n"
			for _, s := range fn {
				p := strings.TrimPrefix(s, box.AbsPath)
				p = strings.TrimPrefix(p, string(filepath.Separator))
				bb.WriteString(fmt.Sprintf(ln, p, makeKey(box, s)))
			}
			return template.HTML(bb.String()), nil
		},
	})

	t, err := t.Parse(diskGlobalTmpl)
	if err != nil {
		return errors.WithStack(err)
	}

	os.MkdirAll(d.DBPath, 0755)
	fp := filepath.Join(d.DBPath, "packed-packr.go")
	f, err := os.Create(fp)
	if err != nil {
		return errors.WithStack(err)
	}
	defer f.Close()

	if err := t.Execute(f, opts); err != nil {
		return errors.WithStack(err)
	}

	ip := filepath.Dir(d.DBPath)
	ip = strings.TrimPrefix(ip, filepath.Join(GoPath(), "src"))
	ip = strings.TrimPrefix(ip, string(filepath.Separator))
	ip = path.Join(ip, d.DBPackage)

	for _, n := range opts.Boxes {
		b := d.boxes[n.Name]
		if b == nil {
			continue
		}

		os.MkdirAll(b.PackageDir, 0755)

		t, err := template.New("").Parse(diskImportTmpl)
		if err != nil {
			return errors.WithStack(err)
		}
		f, err := os.Create(filepath.Join(b.PackageDir, "a_"+b.Package+"-packr.go"))
		if err != nil {
			return errors.WithStack(err)
		}
		defer f.Close()

		o := struct {
			Package string
			Import  string
		}{
			Package: b.Package,
			Import:  ip,
		}
		if err := t.Execute(f, o); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// resolve file paths (only) for the boxes
// compile "global" db
// resolve files for boxes to point at global db
// write global db to disk (default internal/packr)
// write boxes db to disk (default internal/packr)
// write -packr.go files in each package (1 per package) that init the global db

func makeKey(box *parser.Box, path string) string {
	// return path
	// s := strings.TrimPrefix(path, box.AbsPath)
	// return strings.TrimPrefix(s, string(filepath.Separator))
	hasher := md5.New()
	hasher.Write([]byte(path))
	return hex.EncodeToString(hasher.Sum(nil))
}