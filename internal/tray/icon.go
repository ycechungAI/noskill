package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Icon draws the tray dot: grey offline, green idle, amber when something waits. Drawn here so no
// image files ship with the binary.
func Icon(s State) []byte {
	c := map[State]color.RGBA{Offline: {0x8a, 0x8d, 0x93, 0xff}, Idle: {0x2e, 0xa0, 0x63, 0xff}, Attention: {0xe0, 0x9a, 0x1f, 0xff}}[s]
	const n, r = 32, 11.0
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)-15.5, float64(y)-15.5
			d := dx*dx + dy*dy
			switch {
			case d <= (r-1)*(r-1):
				img.SetRGBA(x, y, c)
			case d <= (r+1)*(r+1): // a soft edge
				a := uint8(255 * (1 - (math.Sqrt(d)-(r-1))/2))
				img.SetRGBA(x, y, color.RGBA{c.R * a / 255, c.G * a / 255, c.B * a / 255, a})
			}
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// ICO wraps the PNG in an .ico container, which Windows wants for tray icons (PNG inside ICO has
// worked since Vista).
func ICO(s State) []byte {
	p := Icon(s)
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, 1}) // reserved, type icon, one image
	b.Write([]byte{32, 32, 0, 0})                             // width, height, colours, reserved
	binary.Write(&b, binary.LittleEndian, [2]uint16{1, 32})   // planes, bits per pixel
	binary.Write(&b, binary.LittleEndian, uint32(len(p)))
	binary.Write(&b, binary.LittleEndian, uint32(22)) // offset of the image data
	b.Write(p)
	return b.Bytes()
}
