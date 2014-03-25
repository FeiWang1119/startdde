/**
 * Copyright (c) 2014 Deepin, Inc.
 *               2014 Xu FaSheng
 *
 * Author:      Xu FaSheng <fasheng.xu@gmail.com>
 * Maintainer:  Xu FaSheng <fasheng.xu@gmail.com>
 *
 * This program is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program; if not, see <http://www.gnu.org/licenses/>.
 **/

package main

import (
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"

	"dlib/gio-2.0"
	"fmt"
	"github.com/BurntSushi/xgb/randr"
	"github.com/BurntSushi/xgb/render"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/ewmh"
	"github.com/BurntSushi/xgbutil/xgraphics"
	"github.com/BurntSushi/xgbutil/xprop"
	"github.com/BurntSushi/xgbutil/xwindow"
	"strings"
	"sync"
	"time"
)

const (
	personalizationID           = "com.deepin.dde.personalization"
	gkeyCurrentBackground       = "current-picture"
	deepinBackgroundWindowProp  = "DEEPIN_BACKGROUND_WINDOW"
	deepinBackgroundPixmapProp  = "DEEPIN_BACKGROUND_PIXMAP"
	deepinBackgroundWindowTitle = "Deepin Background"
	defaultBackgroundFile       = "/usr/share/backgrounds/default_background.jpg"
)

var (
	personSettings                = gio.NewSettings(personalizationID)
	XU, _                         = xgbutil.NewConn()
	picFormat24, picFormat32      render.Pictformat
	filterNearest, filterBilinear xproto.Str
	bgwin                         *xwindow.Window
	bgimg                         *xgraphics.Image
	bgimgWidth, bgimgHeight       int
	srcpid, _                     = render.NewPictureId(XU.Conn())
	dstpid, _                     = render.NewPictureId(XU.Conn())
	crtcInfos                     = make(map[randr.Crtc]*crtcInfo)
	srcpidLock                    = sync.Mutex{}
	crtcInfosLock                 = sync.Mutex{}
	drawBgFirstTime               = true
)

type crtcInfo struct {
	x      int16
	y      int16
	width  uint16
	height uint16
}

func initBackground() {
	randr.Init(XU.Conn())
	randr.QueryVersion(XU.Conn(), 1, 4)
	render.Init(XU.Conn())
	render.QueryVersion(XU.Conn(), 0, 11)

	bgwin = createBgWindow(deepinBackgroundWindowTitle)
	queryRender(xproto.Drawable(bgwin.Id))

	// bind picture id to background window
	err := render.CreatePictureChecked(XU.Conn(), dstpid, xproto.Drawable(bgwin.Id), picFormat24, 0, nil).Check()
	if err != nil {
		Logger.Error("create render picture failed: ", err)
	}

	listenBackgroundChanged()
	go listenDisplayChanged()
}

func queryRender(d xproto.Drawable) {
	// get picture formats
	formats, _ := render.QueryPictFormats(XU.Conn()).Reply()
	for _, f := range formats.Formats {
		if f.Depth == 24 && f.Type == render.PictTypeDirect {
			picFormat24 = f.Id
		} else if f.Depth == 32 && f.Type == render.PictTypeDirect {
			picFormat32 = f.Id
		}
	}
	Logger.Debugf("picture format32=%d, format24=%d", picFormat32, picFormat24)

	// get image filters
	filters, _ := render.QueryFilters(XU.Conn(), d).Reply()
	for _, f := range filters.Filters {
		if f.Name == "nearest" {
			filterNearest = f
		} else if f.Name == "bilinear" {
			filterBilinear = f
		}
	}
	Logger.Debug("nearest filter: ", filterNearest)
	Logger.Debug("bilinear filter: ", filterBilinear)
}

func createBgWindow(title string) *xwindow.Window {
	win, err := xwindow.Generate(XU)
	if err != nil {
		Logger.Error("could not generate new window id: ", err)
		return nil
	}
	w, h := getScreenResolution()
	win.Create(XU.RootWin(), 0, 0, int(w), int(h), 0)

	// set _NET_WM_NAME so it looks nice
	err = ewmh.WmNameSet(XU, win.Id, title)
	if err != nil {
		// not a fatal error
		Logger.Error("Could not set _NET_WM_NAME: ", err)
	}

	// set _NET_WM_WINDOW_TYPE_DESKTOP window type
	ewmh.WmWindowTypeSet(XU, win.Id, []string{"_NET_WM_WINDOW_TYPE_DESKTOP"})

	// save background window id to root window property
	xprop.ChangeProp32(XU, XU.RootWin(), deepinBackgroundWindowProp, "WINDOW", uint(win.Id))
	Logger.Debug("background window id: ", win.Id)

	win.Map()
	return win
}

// draw background directly instead of through xrender, for that maybe
// issue with desktop manager after login
func drawBackgroundDirectly() {
	defer func() {
		if err := recover(); err != nil {
			Logger.Error("drawBackgroundDirectly failed: ", err)
		}
	}()
	// TODO
}

func getScreenResolution() (w, h uint16) {
	screen := xproto.Setup(XU.Conn()).DefaultScreen(XU.Conn())
	w, h = screen.WidthInPixels, screen.HeightInPixels
	if w*h == 0 {
		// get root window geometry
		rootRect := xwindow.RootGeometry(XU)
		w, h = uint16(rootRect.Width()), uint16(rootRect.Height())
	}
	if w*h == 0 {
		w, h = 1024, 768 // default value
		Logger.Warningf("get screen resolution failed, use default value: %dx%d", w, h)
	}
	return
}

func updateBackground(delay bool) {
	// load background file
	file, err := os.Open(getBackgroundFile())
	if err != nil {
		Logger.Error("open background failed: ", err)
		return
	}
	defer file.Close()
	img, _, err := image.Decode(file)
	if err != nil {
		Logger.Error("load background failed: ", err)
		return
	}
	bgimgWidth = img.Bounds().Max.X
	bgimgHeight = img.Bounds().Max.Y
	Logger.Debugf("bgimgWidth=%d, bgimgHeight=%d", bgimgWidth, bgimgHeight)

	// convert into an XU image and save pixmap id to root window property
	if bgimg != nil {
		bgimg.Destroy()
	}
	bgimg = xgraphics.NewConvert(XU, img)
	bgimg.CreatePixmap()
	bgimg.XDraw()
	xprop.ChangeProp32(XU, XU.RootWin(), deepinBackgroundPixmapProp, "PIXMAP", uint(bgimg.Pixmap))

	// rebind picture id to background pixmap
	Logger.Debugf("srcpid=%d, dstpid=%d", srcpid, dstpid)
	srcpidLock.Lock()
	render.FreePicture(XU.Conn(), srcpid)
	err = render.CreatePictureChecked(XU.Conn(), srcpid, xproto.Drawable(bgimg.Pixmap), picFormat24, 0, nil).Check()
	if err != nil {
		Logger.Error("create render picture failed: ", err)
		return
	}

	// setup image filter
	err = render.SetPictureFilterChecked(XU.Conn(), srcpid, uint16(filterBilinear.NameLen), filterBilinear.Name, nil).Check()
	if err != nil {
		Logger.Error("set picture filter failed: ", err)
	}
	srcpidLock.Unlock()

	updateAllScreens(delay)
}

func updateAllScreens(delay bool) {
	resources, err := randr.GetScreenResources(XU.Conn(), XU.RootWin()).Reply()
	if err != nil {
		Logger.Error("get scrren resources failed: ", err)
		return
	}

	for _, output := range resources.Outputs {
		reply, err := randr.GetOutputInfo(XU.Conn(), output, 0).Reply()
		if err != nil {
			Logger.Warningf("get output info failed: ", err)
			continue
		}
		if reply.Connection != randr.ConnectionConnected {
			continue
		}
		crtcReply, err := randr.GetCrtcInfo(XU.Conn(), reply.Crtc, 0).Reply()
		if err != nil {
			Logger.Warningf("get crtc info failed: id %d, %v", reply.Crtc, err)
			continue
		}
		updateCrtcInfos(reply.Crtc, crtcReply.X, crtcReply.Y, crtcReply.Width, crtcReply.Height)
		updateScreen(reply.Crtc, delay)
	}
}

func resizeBgWindow(w, h int) {
	geom, _ := bgwin.Geometry()
	if geom.Width() == w && geom.Height() == h {
		return
	}
	Logger.Debugf("background window before resizing, %dx%d", geom.Width(), geom.Height())
	bgwin.MoveResize(0, 0, w, h)
	geom, _ = bgwin.Geometry()
	Logger.Debugf("background window after resizing, %dx%d", geom.Width(), geom.Height())
}

func updateCrtcInfos(crtc randr.Crtc, x, y int16, width, height uint16) (needRedraw bool) {
	crtcInfosLock.Lock()
	defer crtcInfosLock.Unlock()
	needRedraw = false
	if i, ok := crtcInfos[crtc]; ok {
		// current crtc info has been saved,
		// redraw background only when crtc information changed
		if i.x != x || i.y != y || i.width != width || i.height != height {
			// update crtc info and redraw background
			Logger.Debug("update crtc info, old: ", crtcInfos[crtc])
			i.x = x
			i.y = y
			i.width = width
			i.height = height
			needRedraw = true
			Logger.Debug("update crtc info, new: ", crtcInfos[crtc])
		}
	} else {
		// current crtc info is out of save
		crtcInfos[crtc] = &crtcInfo{
			x:      x,
			y:      y,
			width:  width,
			height: height,
		}
		needRedraw = true
		Logger.Debug("add crtc info: ", crtcInfos[crtc])
	}
	return
}

func removeCrtcInfos(crtc randr.Crtc) {
	crtcInfosLock.Lock()
	defer crtcInfosLock.Unlock()
	Logger.Debug("remove crtc info: ", crtcInfos[crtc])
	delete(crtcInfos, crtc)
}

func updateScreen(crtc randr.Crtc, delay bool) {
	if !delay {
		go drawBackgroundByRender(srcpid, dstpid, crtc)
	} else {
		go func() {
			// sleep 1s to ensure window resize event effected
			time.Sleep(1 * time.Second)
			drawBackgroundByRender(srcpid, dstpid, crtc)

			// sleep 5s and redraw background
			time.Sleep(5 * time.Second)
			drawBackgroundByRender(srcpid, dstpid, crtc)
		}()
	}
}

func drawBackgroundByRender(srcpid, dstpid render.Picture, crtc randr.Crtc) {
	defer func() {
		if err := recover(); err != nil {
			Logger.Error("drawBackgroundByRender() failed: ", err)
		}
	}()

	srcpidLock.Lock()
	crtcInfosLock.Lock()
	defer srcpidLock.Unlock()
	defer crtcInfosLock.Unlock()

	var x, y int16
	var width, height uint16
	if i, ok := crtcInfos[crtc]; ok {
		x = i.x
		y = i.y
		width = i.width
		height = i.height
	} else {
		Logger.Error("target crtc info is out of save: id=", crtc)
		return
	}
	Logger.Debugf("draw background through xrender: x=%d, y=%d, width=%d, height=%d", x, y, width, height)

	// get clip rectangle
	rect, err := getClipRect(width, height, uint16(bgimgWidth), uint16(bgimgHeight))
	if err != nil {
		panic(err)
	}

	// scale source image and clip rectangle
	sx := float32(width) / float32(rect.Width)
	sy := float32(height) / float32(rect.Height)
	rect.X = int16(float32(rect.X) * sx)
	rect.Y = int16(float32(rect.Y) * sy)
	rect.Width = uint16(float32(rect.Width) * sx)
	rect.Height = uint16(float32(rect.Height) * sx)
	t := getScaleTransform(sx, sy)
	Logger.Debugf("scale transform: sx=%f, sy=%f, %x", sx, sy, t)
	err = render.SetPictureTransformChecked(XU.Conn(), srcpid, t).Check()
	if err != nil {
		panic(err)
	}

	// draw to background window
	err = render.CompositeChecked(XU.Conn(), render.PictOpSrc, srcpid, 0, dstpid,
		rect.X, rect.Y, 0, 0, x, y, width, height).Check()
	if err != nil {
		panic(err)
	}

	// restore source image
	t = getScaleTransform(1/sx, 1/sy)
	err = render.SetPictureTransformChecked(XU.Conn(), srcpid, t).Check()
	if err != nil {
		panic(err)
	}
}

func getScaleTransform(x, y float32) render.Transform {
	return render.Transform{
		float32ToFixed(1 / x), 0, 0,
		0, float32ToFixed(1 / y), 0,
		0, 0, float32ToFixed(1),
	}
}

// convert float32 value to matrix fixed value
func float32ToFixed(f float32) render.Fixed {
	return render.Fixed(f * 65536)
}

// convert matrix fixed value to float32
func fixedToFloat32(f render.Fixed) float32 {
	return float32(f) / 65536
}

// get rectangle in image which with the same scale to reference
// width/heigh, and the rectangle will placed in center.
func getClipRect(refWidth, refHeight, imgWidth, imgHeight uint16) (rect xproto.Rectangle, err error) {
	if refWidth*refHeight == 0 || imgWidth*imgHeight == 0 {
		err = fmt.Errorf("argument is invalid: ", refWidth, refHeight, imgWidth, imgHeight)
		return
	}
	scale := float32(refWidth) / float32(refHeight)
	w := imgWidth
	h := uint16(float32(w) / scale)
	if h < imgHeight {
		offsetY := (imgHeight - h) / 2
		rect.X = int16(0)
		rect.Y = int16(0 + offsetY)
	} else {
		h = imgHeight
		w = uint16(float32(h) * scale)
		offsetX := (imgWidth - w) / 2
		rect.X = int16(0 + offsetX)
		rect.Y = int16(0)
	}
	rect.Width = w
	rect.Height = h
	return
}

func getBackgroundFile() string {
	uri := personSettings.GetString(gkeyCurrentBackground)
	Logger.Debug("background uri: ", uri)
	path, ok := uriToPath(uri)
	if !ok || !isFileExists(path) {
		Logger.Warning("background file is not exists")
		Logger.Warning("use default background: ", defaultBackgroundFile)
		return defaultBackgroundFile
	}
	return path
}

func uriToPath(uri string) (string, bool) {
	tmp := strings.TrimLeft(uri, " ")
	if strings.HasPrefix(tmp, "file://") {
		return tmp[7:], true
	}
	return "", false
}

func isFileExists(file string) bool {
	if _, err := os.Stat(file); err == nil {
		return true
	}
	return false
}

func listenBackgroundChanged() {
	personSettings.Connect("changed", func(s *gio.Settings, key string) {
		switch key {
		case gkeyCurrentBackground:
			Logger.Debug("background value in gsettings changed: ", key)
			go updateBackground(false)
		}
	})
}

func listenDisplayChanged() {
	randr.SelectInput(XU.Conn(), XU.RootWin(), randr.NotifyMaskCrtcChange|randr.NotifyMaskScreenChange)
	for {
		event, err := XU.Conn().WaitForEvent()
		if err != nil {
			continue
		}
		switch eventType := event.(type) {
		case randr.NotifyEvent:
			switch eventType.SubCode {
			case randr.NotifyCrtcChange:
				info := eventType.U.Cc
				if info.Mode != 0 {
					Logger.Debugf("NotifyCrtcChange: update, id=%d, (%d,%d,%d,%d)",
						info.Crtc, info.X, info.Y, info.Width, info.Height)
					needRedraw := updateCrtcInfos(info.Crtc, info.X, info.Y, info.Width, info.Height)
					if needRedraw {
						updateScreen(info.Crtc, true)
					}
					// TODO
					// updateCrtcInfos(info.Crtc, info.X, info.Y, info.Width, info.Height)
					// updateScreen(info.Crtc, true)
				} else {
					Logger.Debugf("NotifyCrtcChange: remove, id=%d, (%d,%d,%d,%d)",
						info.Crtc, info.X, info.Y, info.Width, info.Height)
					removeCrtcInfos(info.Crtc)
				}
				// updateAllScreens(true)
			}
		case randr.ScreenChangeNotifyEvent:
			Logger.Debugf("ScreenChangeNotifyEvent: %dx%d", eventType.Width, eventType.Height)

			// FIXME skip invalid event for window manager issue
			if eventType.Width < 480 && eventType.Height < 640 {
				continue
			}

			resizeBgWindow(int(eventType.Width), int(eventType.Height))
		}
	}
}