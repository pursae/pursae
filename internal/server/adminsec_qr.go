package server

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
)

// A small QR code encoder (ISO/IEC 18004, byte mode, error correction level M, versions 1-40) so
// the authenticator setup can show its otpauth:// URI as an inline SVG without any third-party
// service or script. It follows the well-known structure of Project Nayuki's reference encoder.

var qrECCPerBlockM = [41]int{-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28}

var qrBlocksM = [41]int{-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49}

type qrCode struct {
	ver, size int
	mod, fn   [][]bool // dark modules; function-pattern modules
}

func qrRawModules(ver int) int {
	r := (16*ver+128)*ver + 64
	if ver >= 2 {
		n := ver/7 + 2
		r -= (25*n-10)*n - 55
		if ver >= 7 {
			r -= 36
		}
	}
	return r
}

func qrDataCodewords(ver int) int {
	return qrRawModules(ver)/8 - qrECCPerBlockM[ver]*qrBlocksM[ver]
}

// qrEncode builds the symbol for data in byte mode at level M, in the smallest version that fits.
func qrEncode(data []byte) (*qrCode, error) {
	ver := 0
	for v := 1; v <= 40; v++ {
		cc := 8
		if v >= 10 {
			cc = 16
		}
		if 4+cc+8*len(data) <= qrDataCodewords(v)*8 {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, errors.New("qr: data too long")
	}
	var bits []bool
	put := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, (v>>i)&1 == 1)
		}
	}
	put(0x4, 4)
	if ver >= 10 {
		put(len(data), 16)
	} else {
		put(len(data), 8)
	}
	for _, b := range data {
		put(int(b), 8)
	}
	capBits := qrDataCodewords(ver) * 8
	t := capBits - len(bits)
	if t > 4 {
		t = 4
	}
	put(0, t)
	put(0, (8-len(bits)%8)%8)
	for pad := 0xEC; len(bits) < capBits; pad ^= 0xEC ^ 0x11 {
		put(pad, 8)
	}
	words := make([]byte, len(bits)/8)
	for i, b := range bits {
		if b {
			words[i>>3] |= 1 << (7 - uint(i&7))
		}
	}
	q := &qrCode{ver: ver, size: ver*4 + 17}
	q.mod, q.fn = make([][]bool, q.size), make([][]bool, q.size)
	for i := range q.mod {
		q.mod[i], q.fn[i] = make([]bool, q.size), make([]bool, q.size)
	}
	q.drawFunctions()
	q.drawCodewords(q.interleave(words))
	best, bestScore := 0, -1
	for m := 0; m < 8; m++ {
		q.applyMask(m)
		q.drawFormat(m)
		if p := q.penalty(); bestScore < 0 || p < bestScore {
			best, bestScore = m, p
		}
		q.applyMask(m) // XOR again undoes it
	}
	q.applyMask(best)
	q.drawFormat(best)
	return q, nil
}

func (q *qrCode) set(x, y int, dark bool) {
	q.mod[y][x] = dark
	q.fn[y][x] = true
}

func (q *qrCode) alignPositions() []int {
	if q.ver == 1 {
		return nil
	}
	n := q.ver/7 + 2
	step := (q.ver*8 + n*3 + 5) / (n*4 - 4) * 2
	out := make([]int, n)
	out[0] = 6
	for i, pos := n-1, q.size-7; i >= 1; i, pos = i-1, pos-step {
		out[i] = pos
	}
	return out
}

func (q *qrCode) drawFunctions() {
	for i := 0; i < q.size; i++ {
		q.set(6, i, i%2 == 0)
		q.set(i, 6, i%2 == 0)
	}
	finder := func(cx, cy int) {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := cx+dx, cy+dy
				if x < 0 || y < 0 || x >= q.size || y >= q.size {
					continue
				}
				d := max(abs(dx), abs(dy))
				q.set(x, y, d != 2 && d != 4)
			}
		}
	}
	finder(3, 3)
	finder(q.size-4, 3)
	finder(3, q.size-4)
	al := q.alignPositions()
	for i := range al {
		for j := range al {
			if (i == 0 && j == 0) || (i == 0 && j == len(al)-1) || (i == len(al)-1 && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					q.set(al[i]+dx, al[j]+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	q.drawFormat(0) // reserve the areas; the real bits come with the mask
	if q.ver >= 7 {
		rem := q.ver
		for i := 0; i < 12; i++ {
			rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
		}
		bits := q.ver<<12 | rem
		for i := 0; i < 18; i++ {
			dark := (bits>>i)&1 == 1
			a, b := q.size-11+i%3, i/3
			q.set(a, b, dark)
			q.set(b, a, dark)
		}
	}
}

func (q *qrCode) drawFormat(mask int) {
	data := 0<<3 | mask // level M has format bits 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return (bits>>i)&1 == 1 }
	for i := 0; i <= 5; i++ {
		q.set(8, i, bit(i))
	}
	q.set(8, 7, bit(6))
	q.set(8, 8, bit(7))
	q.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.set(q.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.set(8, q.size-15+i, bit(i))
	}
	q.set(8, q.size-8, true)
}

func qrMul(x, y byte) byte {
	z := 0
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11D)
		z ^= int((y>>uint(i))&1) * int(x)
	}
	return byte(z)
}

func qrDivisor(degree int) []byte {
	r := make([]byte, degree)
	r[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := 0; j < degree; j++ {
			r[j] = qrMul(r[j], root)
			if j+1 < degree {
				r[j] ^= r[j+1]
			}
		}
		root = qrMul(root, 0x02)
	}
	return r
}

func qrRemainder(data, div []byte) []byte {
	r := make([]byte, len(div))
	for _, b := range data {
		f := b ^ r[0]
		copy(r, r[1:])
		r[len(r)-1] = 0
		for i, c := range div {
			r[i] ^= qrMul(c, f)
		}
	}
	return r
}

func (q *qrCode) interleave(data []byte) []byte {
	nb, ecl := qrBlocksM[q.ver], qrECCPerBlockM[q.ver]
	raw := qrRawModules(q.ver) / 8
	short := nb - raw%nb
	shortLen := raw / nb
	div := qrDivisor(ecl)
	var blocks [][]byte
	k := 0
	for i := 0; i < nb; i++ {
		n := shortLen - ecl
		if i >= short {
			n++
		}
		dat := append([]byte(nil), data[k:k+n]...)
		k += n
		ecc := qrRemainder(dat, div)
		if i < short {
			dat = append(dat, 0)
		}
		blocks = append(blocks, append(dat, ecc...))
	}
	var out []byte
	for i := range blocks[0] {
		for j, b := range blocks {
			if i != shortLen-ecl || j >= short {
				out = append(out, b[i])
			}
		}
	}
	return out
}

func (q *qrCode) drawCodewords(data []byte) {
	i := 0
	for right := q.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < q.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = q.size - 1 - vert
				}
				if !q.fn[y][x] && i < len(data)*8 {
					q.mod[y][x] = (data[i>>3]>>(7-uint(i&7)))&1 == 1
					i++
				}
			}
		}
	}
}

func (q *qrCode) applyMask(m int) {
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			if q.fn[y][x] {
				continue
			}
			var inv bool
			switch m {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			case 7:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv {
				q.mod[y][x] = !q.mod[y][x]
			}
		}
	}
}

// penalty is the standard's mask evaluation (runs, 2x2 blocks, finder-like patterns, balance).
func (q *qrCode) penalty() int {
	n := q.size
	p := 0
	at := func(x, y int, horiz bool) bool {
		if horiz {
			return q.mod[y][x]
		}
		return q.mod[x][y]
	}
	for _, horiz := range []bool{true, false} {
		for a := 0; a < n; a++ {
			run := 1
			for b := 1; b < n; b++ {
				if at(b, a, horiz) == at(b-1, a, horiz) {
					run++
					if run == 5 {
						p += 3
					} else if run > 5 {
						p++
					}
				} else {
					run = 1
				}
			}
			// 1:1:3:1:1 finder-like pattern with four light modules on one side
			for b := 0; b+10 < n; b++ {
				pat := []bool{true, false, true, true, true, false, true}
				ok := true
				for k, v := range pat {
					if at(b+k, a, horiz) != v {
						ok = false
						break
					}
				}
				if !ok {
					continue
				}
				light := func(from int) bool {
					for k := from; k < from+4; k++ {
						if k < 0 || k >= n {
							continue
						}
						if at(k, a, horiz) {
							return false
						}
					}
					return true
				}
				if light(b-4) || light(b+7) {
					p += 40
				}
			}
		}
	}
	for y := 0; y < n-1; y++ {
		for x := 0; x < n-1; x++ {
			c := q.mod[y][x]
			if c == q.mod[y][x+1] && c == q.mod[y+1][x] && c == q.mod[y+1][x+1] {
				p += 3
			}
		}
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if q.mod[y][x] {
				dark++
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10)+total-1)/total - 1
	if k > 0 {
		p += k * 10
	}
	return p
}

// SVG draws the symbol with a four-module quiet zone, one path, scaled by CSS.
func (q *qrCode) SVG(px int) string {
	const quiet = 4
	dim := q.size + quiet*2
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges" role="img" aria-label="QR code">`, dim, dim, px, px)
	fmt.Fprintf(&sb, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, dim, dim)
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			if q.mod[y][x] {
				fmt.Fprintf(&sb, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	sb.WriteString(`"/></svg>`)
	return sb.String()
}

// PNG draws the symbol with a four-module quiet zone, each module scale pixels wide, as a
// two-colour image (for downloads; pages show the SVG).
func (q *qrCode) PNG(scale int) []byte {
	const quiet = 4
	if scale < 1 {
		scale = 1
	}
	dim := (q.size + quiet*2) * scale
	img := image.NewPaletted(image.Rect(0, 0, dim, dim), color.Palette{color.White, color.Black})
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			if !q.mod[y][x] {
				continue
			}
			for dy := 0; dy < scale; dy++ {
				row := img.Pix[((y+quiet)*scale+dy)*img.Stride+(x+quiet)*scale:]
				for dx := 0; dx < scale; dx++ {
					row[dx] = 1
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}
