//go:generate gopherjs build -m

package main

import (
	"archive/zip"
	"bufio"
	"crypto/md5"
	"errors"
	"fmt"
	"image"
	"io"
	"io/ioutil"
	"log"
	"strings"
	"time"

	"github.com/gopherjs/gopherjs/js"
	"github.com/gopherjs/jsbuiltin"
	"github.com/oov/psd"
	"github.com/saintfish/chardet"
	"golang.org/x/text/encoding/japanese"
)

type root struct {
	X        int
	Y        int
	Width    int
	Height   int
	Children []layer

	CanvasWidth  int
	CanvasHeight int
	Hash         string
	PFV          string
	Readme       string

	seq int

	processed  int
	makeCanvas func(l *layer)
	realRect   image.Rectangle

	transferable js.S
}

type layer struct {
	SeqID int

	X        int
	Y        int
	Width    int
	Height   int
	Children []layer

	A    *js.Object
	R    *js.Object
	G    *js.Object
	B    *js.Object
	Mask *js.Object

	Name                  string
	BlendMode             string
	Opacity               uint8
	Clipping              bool
	BlendClippedElements  bool
	TransparencyProtected bool
	Visible               bool
	//Canvas                *js.Object
	MaskX      int
	MaskY      int
	MaskWidth  int
	MaskHeight int
	//MaskCanvas            *js.Object
	MaskDefaultColor int
	Folder           bool
	FolderOpen       bool
	psdLayer         *psd.Layer
}

func main() {
	// psd.Debug = log.New(os.Stdout, "psd: ", log.Lshortfile)
	if js.Global.Get("importScripts").Bool() {
		workerMain()
	} else {
		mainMain()
	}
}

func (r *root) buildLayer(l *layer) error {
	var err error

	r.seq++
	l.SeqID = r.seq

	if l.psdLayer.UnicodeName == "" && l.psdLayer.MBCSName != "" {
		if l.Name, err = japanese.ShiftJIS.NewDecoder().String(l.psdLayer.MBCSName); err != nil {
			l.Name = l.psdLayer.MBCSName
		}
	} else {
		l.Name = l.psdLayer.UnicodeName
	}
	if l.psdLayer.Folder() {
		l.BlendMode = l.psdLayer.SectionDividerSetting.BlendMode.String()
	} else {
		l.BlendMode = l.psdLayer.BlendMode.String()
	}
	l.Opacity = l.psdLayer.Opacity
	l.Clipping = l.psdLayer.Clipping
	l.BlendClippedElements = l.psdLayer.BlendClippedElements
	l.Visible = l.psdLayer.Visible()
	l.Folder = l.psdLayer.Folder()
	l.FolderOpen = l.psdLayer.FolderIsOpen()

	if l.psdLayer.HasImage() && l.psdLayer.Rect.Dx()*l.psdLayer.Rect.Dy() > 0 {
		l.R = js.NewArrayBuffer(l.psdLayer.Channel[0].Data)
		l.G = js.NewArrayBuffer(l.psdLayer.Channel[1].Data)
		l.B = js.NewArrayBuffer(l.psdLayer.Channel[2].Data)
		r.transferable = append(r.transferable, l.R, l.G, l.B)
		if a, ok := l.psdLayer.Channel[-1]; ok {
			l.A = js.NewArrayBuffer(a.Data)
			r.transferable = append(r.transferable, l.A)
		}
		r.realRect = r.realRect.Union(l.psdLayer.Rect)
	}
	l.MaskX = l.psdLayer.Mask.Rect.Min.X
	l.MaskY = l.psdLayer.Mask.Rect.Min.Y
	l.MaskWidth = l.psdLayer.Mask.Rect.Dx()
	l.MaskHeight = l.psdLayer.Mask.Rect.Dy()
	l.MaskDefaultColor = l.psdLayer.Mask.DefaultColor
	if _, ok := l.psdLayer.Channel[-2]; ok && l.psdLayer.Mask.Enabled() && l.MaskWidth*l.MaskHeight > 0 {
		l.Mask = js.NewArrayBuffer(l.psdLayer.Channel[-2].Data)
		r.transferable = append(r.transferable, l.Mask)
	}

	r.processed++
	if r.makeCanvas != nil {
		r.makeCanvas(l)
	}

	rect := l.psdLayer.Rect
	for i := range l.psdLayer.Layer {
		l.Children = append(l.Children, layer{psdLayer: &l.psdLayer.Layer[i]})
		if err = r.buildLayer(&l.Children[i]); err != nil {
			return err
		}
		rect = rect.Union(image.Rect(
			l.Children[i].X,
			l.Children[i].Y,
			l.Children[i].X+l.Children[i].Width,
			l.Children[i].Y+l.Children[i].Height,
		))
	}
	l.X = rect.Min.X
	l.Y = rect.Min.Y
	l.Width = rect.Dx()
	l.Height = rect.Dy()
	return nil
}

func (r *root) Build(img *psd.PSD) error {
	r.CanvasWidth = img.Config.Rect.Dx()
	r.CanvasHeight = img.Config.Rect.Dy()
	for i := range img.Layer {
		r.Children = append(r.Children, layer{psdLayer: &img.Layer[i]})
		if err := r.buildLayer(&r.Children[i]); err != nil {
			return err
		}
	}
	r.realRect = r.realRect.Intersect(image.Rect(0, 0, r.CanvasWidth, r.CanvasHeight))
	r.X = r.realRect.Min.X
	r.Y = r.realRect.Min.Y
	r.Width = r.realRect.Dx()
	r.Height = r.realRect.Dy()
	return nil
}

func readTextFile(r io.Reader) (string, error) {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return "", err
	}

	d, err := chardet.NewTextDetector().DetectBest(b)
	if err != nil {
		return "", err
	}

	switch d.Charset {
	case "ISO-2022-JP":
		b, err = japanese.ISO2022JP.NewDecoder().Bytes(b)
	case "EUC-JP":
		b, err = japanese.EUCJP.NewDecoder().Bytes(b)
	case "Shift_JIS":
		b, err = japanese.ShiftJIS.NewDecoder().Bytes(b)
	case "UTF-8":
		break
	default:
		return "", errors.New("unsupported charset: " + d.Charset)
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type reader interface {
	io.Reader
	Sum() []byte
}

func parse(rd readerAt, progress func(progress float64)) (*root, error) {
	var r root
	s := time.Now().UnixNano()

	if rd.Size() < 4 {
		return nil, errors.New("unsupported file type")
	}
	var head [4]byte
	_, err := rd.ReadAt(head[:], 0)
	if err != nil {
		return nil, err
	}
	var reader reader
	switch string(head[:]) {
	case "PK\x03\x04": // zip archive
		zr, err := zip.NewReader(rd, rd.Size())
		if err != nil {
			return nil, err
		}
		var psdf, pfvf, txtf *zip.File
		for _, f := range zr.File {
			if psdf == nil && strings.ToLower(f.Name[len(f.Name)-4:]) == ".psd" {
				psdf = f
				continue
			}
			if pfvf == nil && strings.ToLower(f.Name[len(f.Name)-4:]) == ".pfv" {
				pfvf = f
				continue
			}
			if txtf == nil && strings.ToLower(f.Name[len(f.Name)-4:]) == ".txt" {
				txtf = f
				continue
			}
		}
		if psdf == nil {
			return nil, errors.New("psd file is not found from given zip archive")
		}

		if pfvf != nil {
			pfvr, err := pfvf.Open()
			if err != nil {
				return nil, err
			}
			defer pfvr.Close()
			r.PFV, err = readTextFile(pfvr)
			if err != nil {
				return nil, err
			}
		}

		if txtf != nil {
			txtr, err := txtf.Open()
			if err != nil {
				return nil, err
			}
			defer txtr.Close()
			r.Readme, err = readTextFile(txtr)
			if err != nil {
				return nil, err
			}
		}

		rc, err := psdf.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		reader = &genericProgressReader{
			R:        rc,
			Hash:     md5.New(),
			Progress: progress,
			size:     int(psdf.UncompressedSize64),
		}
	case "7z\xbc\xaf": // 7z archive
		return nil, errors.New("7z archive is not supported")
	case "8BPS": // psd file
		reader = &genericProgressReader{
			R:        bufio.NewReaderSize(rd, 1024*1024*2),
			Hash:     md5.New(),
			Progress: progress,
			size:     int(rd.Size()),
		}
		break
	default:
		return nil, errors.New("unsupported file type")
	}
	psdImg, _, err := psd.Decode(reader, nil)
	if err != nil {
		return nil, err
	}
	e := time.Now().UnixNano()
	progress(1)
	log.Println("decode PSD structure:", (e-s)/1e6)

	if psdImg.Config.ColorMode != psd.ColorModeRGB {
		return nil, errors.New("Unsupported color mode")
	}

	s = time.Now().UnixNano()
	r.Hash = fmt.Sprintf("%x", reader.Sum())
	if err = r.Build(psdImg); err != nil {
		return nil, err
	}
	e = time.Now().UnixNano()
	log.Println("build layer tree:", (e-s)/1e6)
	return &r, nil
}

func parsePSD(in *js.Object, progress *js.Object, complete *js.Object, failed *js.Object) {
	go func() {
		r, err := newReaderFromJSObject(in)
		if err != nil {
			failed.Invoke(err.Error())
			return
		}
		next := time.Now()
		root, err := parse(r, func(prog float64) {
			if now := time.Now(); now.After(next) {
				progress.Invoke(prog)
				time.Sleep(1) // anti-freeze
				next = now.Add(100 * time.Millisecond)
			}
		})
		if err != nil {
			failed.Invoke(err.Error())
			return
		}
		complete.Invoke(root)
	}()
}

func parsePSDInWorker(in *js.Object, progress *js.Object, complete *js.Object, failed *js.Object) {
	script := js.Global.Get("document").Call("getElementById", "psdgo")
	if !script.Bool() {
		panic("id=psdgo not found")
	}
	worker := js.Global.Get("Worker").New(script.Get("src"))
	worker.Set("onmessage", func(e *js.Object) {
		data := e.Get("data")
		switch data.Get("type").String() {
		case "progress":
			progress.Invoke(data.Get("progress"))
		case "error":
			failed.Invoke(data.Get("error"))
		case "complete":
			complete.Invoke(data.Get("root"))
		}
	})
	if jsbuiltin.InstanceOf(in, js.Global.Get("ArrayBuffer")) {
		worker.Call("postMessage", js.M{
			"input": in,
		}, js.S{in})
	} else {
		worker.Call("postMessage", js.M{"input": in})
	}
}

func mainMain() {
	js.Global.Set("PSD", js.M{
		"parse":       parsePSD,
		"parseWorker": parsePSDInWorker,
	})
}

func workerMain() {
	js.Global.Set("onmessage", func(e *js.Object) {
		data := e.Get("data")
		input := data.Get("input")
		r, err := newReaderFromJSObject(input)
		if err != nil {
			js.Global.Call("postMessage", js.M{"type": "error", "error": err.Error()})
			return
		}
		go func() {
			next := time.Now()
			root, err := parse(r, func(progress float64) {
				if now := time.Now(); now.After(next) {
					js.Global.Call("postMessage", js.M{
						"type":     "progress",
						"progress": progress,
					})
					next = now.Add(100 * time.Millisecond)
				}
			})
			if err != nil {
				js.Global.Call("postMessage", js.M{"type": "error", "error": err.Error()})
				return
			}
			js.Global.Call("postMessage", js.M{
				"type": "complete",
				"root": root,
			}, root.transferable)
		}()
	})
}
