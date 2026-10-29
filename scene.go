package main

// Native scene support. go-rmscene is MIT licensed, Copyright (c) 2026
// Alex Gorbatchev and upstream contributors. Its complete notice is retained
// by the Go module dependency; distribute dependency licenses with binaries.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	rms "github.com/alexgorbatchev/go-rmscene"
	"html"
	"io"
	"math"
	"sort"
	"strings"
)

type sceneField struct {
	tag  uint64
	data []byte
}

func sceneVar(b []byte) (uint64, int, error) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	return v, n, nil
}
func sceneFields(b []byte) (map[int]sceneField, error) {
	out := map[int]sceneField{}
	for len(b) > 0 {
		t, n, e := sceneVar(b)
		if e != nil {
			return nil, e
		}
		b = b[n:]
		size := 0
		switch t & 15 {
		case 1:
			size = 1
		case 4:
			size = 4
		case 8:
			size = 8
		case 15:
			if len(b) < 1 {
				return nil, io.ErrUnexpectedEOF
			}
			_, n, e = sceneVar(b[1:])
			if e != nil {
				return nil, e
			}
			size = 1 + n
		case 12:
			if len(b) < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			size = int(binary.LittleEndian.Uint32(b))
			b = b[4:]
		default:
			return nil, fmt.Errorf("unknown scene tag %x", t)
		}
		if size > len(b) {
			return nil, io.ErrUnexpectedEOF
		}
		out[int(t>>4)] = sceneField{t, b[:size]}
		b = b[size:]
	}
	return out, nil
}
func sceneID(b []byte) rms.CrdtId {
	if len(b) < 2 {
		return rms.CrdtId{}
	}
	v, _, _ := sceneVar(b[1:])
	return rms.CrdtId{Part1: b[0], Part2: v}
}
func sceneString(b []byte) string {
	v, n, e := sceneVar(b)
	if e != nil || len(b) < n+1+int(v) {
		return ""
	}
	return string(b[n+1 : n+1+int(v)])
}
func sceneUint(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}
func sceneDouble(b []byte) float64 {
	if len(b) < 8 {
		return 0
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b))
}

type sceneChar struct {
	id   rms.CrdtId
	text string
}
type sceneText struct {
	chars       []sceneChar
	styles      map[rms.CrdtId]byte
	x, y, width float64
}

func readSceneText(b []byte) (sceneText, error) {
	t := sceneText{styles: map[rms.CrdtId]byte{}}
	f, e := sceneFields(b)
	if e != nil {
		return t, e
	}
	g, e := sceneFields(f[2].data)
	if e != nil {
		return t, e
	}
	h, e := sceneFields(g[1].data)
	if e != nil {
		return t, e
	}
	raw := h[1].data
	count, n, e := sceneVar(raw)
	if e != nil {
		return t, e
	}
	raw = raw[n:]
	type item struct {
		id, left, right rms.CrdtId
		text            string
		del             uint32
	}
	items := map[rms.CrdtId]item{}
	for i := uint64(0); i < count; i++ {
		tag, n, e := sceneVar(raw)
		if e != nil || tag != 12 || len(raw) < n+4 {
			return t, fmt.Errorf("invalid text item")
		}
		size := int(sceneUint(raw[n:]))
		raw = raw[n+4:]
		if size > len(raw) {
			return t, io.ErrUnexpectedEOF
		}
		q, e := sceneFields(raw[:size])
		if e != nil {
			return t, e
		}
		raw = raw[size:]
		id := sceneID(q[2].data)
		left, right := sceneID(q[3].data), sceneID(q[4].data)
		value, deleted := []rune(sceneString(q[6].data)), sceneUint(q[5].data)
		length := len(value)
		if deleted > 0 {
			if deleted > 1000000 {
				return t, fmt.Errorf("text deletion exceeds scene limit")
			}
			length = int(deleted)
		}
		// Expand compressed spans before ordering: later edits may refer to
		// any implicit character ID inside an earlier span.
		for j := 0; j < length; j++ {
			charID := rms.CrdtId{Part1: id.Part1, Part2: id.Part2 + uint64(j)}
			next := right
			if j+1 < length {
				next = rms.CrdtId{Part1: id.Part1, Part2: charID.Part2 + 1}
			}
			text := ""
			del := uint32(0)
			if deleted > 0 {
				del = 1
			} else {
				text = string(value[j])
			}
			items[charID] = item{charID, left, next, text, del}
			left = charID
		}
		if length == 0 && len(q[6].data) > 0 {
			items[id] = item{id, left, right, "", 0}
		}
	}
	// Topological CRDT ordering, including right-neighbor constraints.
	edges := map[rms.CrdtId][]rms.CrdtId{}
	degree := map[rms.CrdtId]int{}
	for id, it := range items {
		degree[id] = 0
		_ = it
	}
	for id, it := range items {
		if _, ok := items[it.left]; ok {
			edges[it.left] = append(edges[it.left], id)
			degree[id]++
		}
		if _, ok := items[it.right]; ok {
			edges[id] = append(edges[id], it.right)
			degree[it.right]++
		}
	}
	for len(degree) > 0 {
		ready := []rms.CrdtId{}
		for id, d := range degree {
			if d == 0 {
				ready = append(ready, id)
			}
		}
		if len(ready) == 0 {
			return t, fmt.Errorf("cyclic text CRDT")
		}
		sort.Slice(ready, func(i, j int) bool {
			if ready[i].Part1 != ready[j].Part1 {
				return ready[i].Part1 > ready[j].Part1
			}
			return ready[i].Part2 < ready[j].Part2
		})
		id := ready[0]
		it := items[id]
		if it.del == 0 {
			for j, r := range []rune(it.text) {
				t.chars = append(t.chars, sceneChar{rms.CrdtId{Part1: id.Part1, Part2: id.Part2 + uint64(j)}, string(r)})
			}
		}
		delete(degree, id)
		for _, next := range edges[id] {
			degree[next]--
		}
	}
	h, e = sceneFields(g[2].data)
	if e == nil {
		raw = h[1].data
		count, n, e = sceneVar(raw)
		if e == nil {
			raw = raw[n:]
			for i := uint64(0); i < count; i++ {
				if len(raw) < 2 {
					break
				}
				_, n, e = sceneVar(raw[1:])
				if e != nil {
					break
				}
				id := sceneID(raw[:n+1])
				raw = raw[n+1:]
				_, n, e = sceneVar(raw)
				if e != nil || len(raw) < n+1 {
					break
				}
				raw = raw[n:]
				_, n, e = sceneVar(raw[1:])
				if e != nil {
					break
				}
				raw = raw[n+1:]
				tag, n, e := sceneVar(raw)
				if e != nil || tag != 44 || len(raw) < n+4 {
					break
				}
				size := int(sceneUint(raw[n:]))
				raw = raw[n+4:]
				if size > len(raw) || size < 2 {
					break
				}
				t.styles[id] = raw[1]
				raw = raw[size:]
			}
		}
	}
	p := f[3].data
	if len(p) >= 16 {
		t.x = sceneDouble(p)
		t.y = sceneDouble(p[8:])
	}
	t.width = float64(math.Float32frombits(sceneUint(f[4].data)))
	return t, nil
}

type sceneGlyph struct {
	text   string
	color  int
	rects  [][4]float64
	parent rms.CrdtId
}
type nativeScene struct {
	blocks       []rms.Block
	texts        []sceneText
	glyphs       []sceneGlyph
	w, h         float64
	legacy       bool
	legacyWidths map[*rms.Line]float64
}

func parseNativeScene(data []byte) (*nativeScene, error) {
	s := &nativeScene{w: 1404, h: 1872}
	if len(data) < 43 {
		return nil, fmt.Errorf("truncated rm header")
	}
	if !bytes.Contains(data[:43], []byte("version=6")) {
		return parseLegacyScene(data)
	}
	// Upstream currently discards SceneInfo's unrecognized trailing fields,
	// including paper_size, and requires optional visibility fields. Inspect
	// that block ourselves, preserving the original file for append-only edits.
	filtered := append([]byte{}, data[:43]...)
	var sceneInfo []rms.Block
	for raw := data[43:]; len(raw) > 0; {
		if len(raw) < 8 {
			return nil, io.ErrUnexpectedEOF
		}
		size := int(sceneUint(raw))
		if size > len(raw)-8 {
			return nil, io.ErrUnexpectedEOF
		}
		block := raw[:size+8]
		if raw[7] == 13 {
			f, e := sceneFields(raw[8 : size+8])
			if e != nil {
				return nil, fmt.Errorf("scene info: %w", e)
			}
			if len(f[5].data) >= 8 {
				s.w = float64(sceneUint(f[5].data))
				s.h = float64(sceneUint(f[5].data[4:]))
			}
			sceneInfo = append(sceneInfo, &rms.UnknownBlock{Type: 13, Data: append([]byte{}, raw[8:size+8]...)})
		} else {
			filtered = append(filtered, block...)
		}
		raw = raw[size+8:]
	}
	parsed, e := rms.Parse(filtered)
	if e != nil {
		return nil, e
	}
	s.blocks = append(parsed.Blocks, sceneInfo...)
	for _, b := range s.blocks {
		switch v := b.(type) {
		case *rms.UnknownBlock:
			if v.Type == 7 {
				t, e := readSceneText(v.Data)
				if e != nil {
					return nil, fmt.Errorf("root text: %w", e)
				}
				s.texts = append(s.texts, t)
			}
			if v.Type == 3 {
				f, e := sceneFields(v.Data)
				if e != nil {
					return nil, e
				}
				value := f[6].data
				if len(value) == 0 || sceneUint(f[5].data) > 0 {
					continue
				}
				q, e := sceneFields(value[1:])
				if e != nil {
					return nil, e
				}
				g := sceneGlyph{text: sceneString(q[5].data), color: int(sceneUint(q[4].data)), parent: sceneID(f[1].data)}
				r := q[6].data
				count, n, e := sceneVar(r)
				if e != nil {
					return nil, e
				}
				r = r[n:]
				if count > uint64(len(r)/32) {
					return nil, io.ErrUnexpectedEOF
				}
				for i := uint64(0); i < count; i++ {
					g.rects = append(g.rects, [4]float64{sceneDouble(r), sceneDouble(r[8:]), sceneDouble(r[16:]), sceneDouble(r[24:])})
					r = r[32:]
				}
				s.glyphs = append(s.glyphs, g)
			}
		}
	}
	if s.w <= 0 || s.h <= 0 || s.w > 50000 || s.h > 50000 {
		return nil, fmt.Errorf("invalid scene dimensions")
	}
	return s, nil
}
func parseLegacyScene(data []byte) (*nativeScene, error) {
	version := 0
	for _, v := range []int{3, 4, 5} {
		if bytes.Contains(data[:43], []byte(fmt.Sprintf("version=%d", v))) {
			version = v
		}
	}
	if version == 0 {
		return nil, fmt.Errorf("unsupported rm version")
	}
	s := &nativeScene{w: 1404, h: 1872, legacy: true, legacyWidths: map[*rms.Line]float64{}}
	r := bytes.NewReader(data[43:])
	read := func() (uint32, error) { var n uint32; e := binary.Read(r, binary.LittleEndian, &n); return n, e }
	layers, e := read()
	if e != nil || layers > 10000 {
		return nil, fmt.Errorf("invalid layer count")
	}
	for l := uint32(0); l < layers; l++ {
		count, e := read()
		if e != nil || count > 100000 {
			return nil, fmt.Errorf("invalid stroke count")
		}
		for j := uint32(0); j < count; j++ {
			var pen, col, pad uint32
			var width, unknown float32
			for _, v := range []any{&pen, &col, &pad, &width} {
				if e = binary.Read(r, binary.LittleEndian, v); e != nil {
					return nil, e
				}
			}
			if version == 5 {
				if e = binary.Read(r, binary.LittleEndian, &unknown); e != nil {
					return nil, e
				}
			}
			n, e := read()
			if e != nil || uint64(n)*24 > uint64(r.Len()) {
				return nil, fmt.Errorf("invalid point count")
			}
			line := &rms.Line{Tool: rms.PenTool(pen), Color: rms.PenColor(col), ThicknessScale: float64(width)}
			for k := uint32(0); k < n; k++ {
				var p [6]float32
				if e = binary.Read(r, binary.LittleEndian, &p); e != nil {
					return nil, e
				}
				line.Points = append(line.Points, rms.Point{X: p[0] - 702, Y: p[1], Width: uint16(math.Max(0, math.Min(65535, float64(p[4])))), Pressure: uint8(math.Max(0, math.Min(255, float64(p[5]*255))))})
				s.legacyWidths[line] += float64(p[4]) / float64(n)
			}
			s.blocks = append(s.blocks, &rms.SceneLineItemBlock{Item: rms.Item[*rms.Line]{Value: line}})
		}
	}
	return s, nil
}

var sceneColors = []string{"black", "#808080", "white", "#FFD700", "#00A000", "#FF69B4", "#4169E1", "#E00000", "#A0A0A0", "#FFD700", "#00C000", "#00CED1", "#FF00FF", "#FFD700"}

func sceneColor(n int) string {
	if n < 0 || n >= len(sceneColors) {
		return "black"
	}
	return sceneColors[n]
}
func (s *nativeScene) elements() (string, [4]float64) {
	var out strings.Builder
	bounds := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	add := func(x, y float64) {
		bounds[0] = math.Min(bounds[0], x)
		bounds[1] = math.Min(bounds[1], y)
		bounds[2] = math.Max(bounds[2], x)
		bounds[3] = math.Max(bounds[3], y)
	}
	anchors := map[rms.CrdtId]float64{}
	for _, t := range s.texts {
		scale := s.h / 1872
		y := -39 * scale
		anchors[rms.CrdtId{Part1: 0, Part2: 281474976710654}] = t.y + y
		style := t.styles[rms.CrdtId{}]
		paras := [][]sceneChar{{}}
		for _, c := range t.chars {
			if c.text == "\n" {
				paras = append(paras, []sceneChar{c})
			} else {
				paras[len(paras)-1] = append(paras[len(paras)-1], c)
			}
		}
		for _, para := range paras {
			for _, c := range para {
				if st, ok := t.styles[c.id]; ok {
					style = st
				}
				anchors[c.id] = t.y + y
			}
			height, size := 70.0*scale, 30.0*scale
			family, weight := "sans-serif", "normal"
			switch style {
			case 2:
				height = 150 * scale
				size = 60 * scale
				family = "serif"
				weight = "bold"
			case 3:
				size = 34 * scale
				weight = "bold"
			case 4, 5, 6, 7:
				height = 35 * scale
			}
			var text strings.Builder
			for _, c := range para {
				text.WriteString(c.text)
			}
			words := strings.Split(strings.TrimSpace(text.String()), " ")
			if strings.TrimSpace(text.String()) == "" {
				words = nil
			}
			if len(words) == 0 {
				y += height
				continue
			}
			limit := int(t.width / (size * .5))
			if limit < 1 {
				limit = 100000
			}
			lines := []string{}
			line := ""
			for _, w := range words {
				if line != "" && len([]rune(line+" "+w)) > limit {
					lines = append(lines, line)
					line = ""
				}
				if line != "" {
					line += " "
				}
				line += w
			}
			lines = append(lines, line)
			for i, line := range lines {
				if i == 0 {
					y += height
				} else {
					y += height * 44 / 70
				}
				fmt.Fprintf(&out, `<text x="%.1f" y="%.1f" font-family="%s" font-size="%.1f" font-weight="%s" fill="black" xml:space="preserve">%s</text>`, t.x, t.y+y, family, size, weight, html.EscapeString(line))
				add(t.x, t.y+y-size)
				add(t.x+math.Min(t.width, float64(len([]rune(line)))*size*.5), t.y+y+size*.3)
			}
		}
		anchors[rms.CrdtId{Part1: 0, Part2: 281474976710655}] = t.y + y
	}
	parents := map[rms.CrdtId]rms.CrdtId{}
	own := map[rms.CrdtId][2]float64{}
	for _, b := range s.blocks {
		switch v := b.(type) {
		case *rms.SceneTreeBlock:
			parents[v.TreeID] = v.ParentID
		case *rms.TreeNodeBlock:
			if v.Group.AnchorID != nil {
				if y, ok := anchors[*v.Group.AnchorID]; ok {
					x := 0.0
					if v.Group.AnchorOriginX != nil {
						x = float64(*v.Group.AnchorOriginX)
					}
					own[v.Group.NodeID] = [2]float64{x, y}
				}
			}
		}
	}
	offset := func(id rms.CrdtId) (float64, float64) {
		x, y := 0.0, 0.0
		seen := map[rms.CrdtId]bool{}
		for !seen[id] {
			seen[id] = true
			v := own[id]
			x += v[0]
			y += v[1]
			p, ok := parents[id]
			if !ok {
				break
			}
			id = p
		}
		return x, y
	}
	for _, b := range s.blocks {
		v, ok := b.(*rms.SceneLineItemBlock)
		if !ok || v.Item.Value == nil || v.Item.DeletedLength > 0 {
			continue
		}
		l := v.Item.Value
		if len(l.Points) == 0 || l.Tool == 6 || l.Tool == 8 || (s.legacy && l.Tool == 7) {
			continue
		}
		dx, dy := offset(v.ParentID)
		width := 0.0
		for _, p := range l.Points {
			width += float64(p.Width)
		}
		width /= float64(len(l.Points))
		if s.legacy {
			width = s.legacyWidths[l]
		}
		opacity := 1.0
		strokeColor := sceneColor(int(l.Color))
		if l.Tool == 5 || l.Tool == 18 {
			width = math.Max(10, math.Min(40, width*2))
			opacity = .35
			if s.legacy {
				strokeColor = "#FFD700"
			}
		} else {
			width = math.Max(.5, math.Min(5, width*.8))
		}
		fmt.Fprintf(&out, `<path fill="none" stroke="%s" stroke-width="%.1f" opacity="%.2f" stroke-linecap="round" stroke-linejoin="round" d="`, strokeColor, width, opacity)
		for i, p := range l.Points {
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			x, y := float64(p.X)+dx, float64(p.Y)+dy
			fmt.Fprintf(&out, "%s %.1f %.1f ", cmd, x, y)
			add(x, y)
		}
		out.WriteString(`"/>`)
	}
	for _, g := range s.glyphs {
		dx, dy := offset(g.parent)
		for _, r := range g.rects {
			fmt.Fprintf(&out, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s" opacity="0.35"/>`, r[0]+dx, r[1]+dy, r[2], r[3], sceneColor(g.color))
			add(r[0]+dx, r[1]+dy)
			add(r[0]+dx+r[2], r[1]+dy+r[3])
		}
	}
	return out.String(), bounds
}
