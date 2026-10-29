package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	rms "github.com/alexgorbatchev/go-rmscene"
	"math"
	"reflect"
	"strings"
)

// NewNotebookPage creates a native v6 drawable scene, including a root text
// CRDT, author UUID, and a real layer. No document files are written here.
func NewNotebookPage(text string) ([]byte, error) {
	return NewNotebookPageWithAuthor(text, "")
}

// NewNotebookPageWithAuthor accepts a canonical UUID from cPages.uuids.
// An empty authorUUID allocates a fresh random UUID.
func NewNotebookPageWithAuthor(text, authorUUID string) ([]byte, error) {
	var buf bytes.Buffer
	w := rms.NewDataWriter(&buf)
	w.WriteHeader()
	zero := rms.CrdtId{}
	id := func(a uint8, b uint64) rms.CrdtId { return rms.CrdtId{Part1: a, Part2: b} }
	uuid := make([]byte, 16)
	if authorUUID == "" {
		if _, e := rand.Read(uuid); e != nil {
			return nil, e
		}
		uuid[6] = (uuid[6] & 15) | 64
		uuid[8] = (uuid[8] & 63) | 128
	} else {
		if len(authorUUID) != 36 || authorUUID[8] != '-' || authorUUID[13] != '-' || authorUUID[18] != '-' || authorUUID[23] != '-' {
			return nil, fmt.Errorf("invalid author UUID")
		}
		var e error
		uuid, e = hex.DecodeString(strings.ReplaceAll(authorUUID, "-", ""))
		if e != nil || len(uuid) != 16 {
			return nil, fmt.Errorf("invalid author UUID")
		}
	}
	// Scene UUIDs are little-endian in their first three integer fields.
	sceneSwapUUID(uuid)
	block := func(kind uint8, f func(*rms.DataWriter) error) error { return w.WriteBlock(kind, 1, 1, f) }
	if e := block(9, func(w *rms.DataWriter) error {
		w.WriteVarUint(1)
		return w.WriteSubblock(0, func(w *rms.DataWriter) error { w.WriteVarUint(16); w.WriteBytes(uuid); return w.WriteUint16(1) })
	}); e != nil {
		return nil, e
	}
	block(0, func(w *rms.DataWriter) error { w.WriteId(1, id(1, 1)); return w.WriteTaggedBool(2, true) })
	block(10, func(w *rms.DataWriter) error {
		w.WriteTaggedInt(1, 1)
		w.WriteTaggedInt(2, 0)
		w.WriteTaggedInt(3, uint32(len([]rune(text))+1))
		return w.WriteTaggedInt(4, uint32(strings.Count(text, "\n")+1))
	})
	block(1, func(w *rms.DataWriter) error {
		w.WriteId(1, id(0, 11))
		w.WriteId(2, zero)
		w.WriteTaggedBool(3, true)
		return w.WriteSubblock(4, func(w *rms.DataWriter) error { return w.WriteId(1, id(0, 1)) })
	})
	block(7, func(w *rms.DataWriter) error {
		w.WriteId(1, zero)
		w.WriteSubblock(2, func(w *rms.DataWriter) error {
			w.WriteSubblock(1, func(w *rms.DataWriter) error {
				return w.WriteSubblock(1, func(w *rms.DataWriter) error {
					w.WriteVarUint(1)
					return w.WriteSubblock(0, func(w *rms.DataWriter) error {
						w.WriteId(2, id(1, 16))
						w.WriteId(3, zero)
						w.WriteId(4, zero)
						w.WriteTaggedInt(5, 0)
						if text != "" {
							return w.WriteString(6, text)
						}
						return nil
					})
				})
			})
			return w.WriteSubblock(2, func(w *rms.DataWriter) error {
				return w.WriteSubblock(1, func(w *rms.DataWriter) error {
					w.WriteVarUint(1)
					w.WriteCrdtId(zero)
					w.WriteId(1, id(1, 15))
					return w.WriteSubblock(2, func(w *rms.DataWriter) error { w.WriteUint8(17); return w.WriteUint8(1) })
				})
			})
		})
		w.WriteSubblock(3, func(w *rms.DataWriter) error { w.WriteFloat64(-468); return w.WriteFloat64(234) })
		return w.WriteTaggedFloat(4, 936)
	})
	for _, n := range []uint64{1, 11} {
		block(2, func(w *rms.DataWriter) error {
			w.WriteId(1, id(0, n))
			if n == 11 {
				w.WriteLwwString(2, id(0, 12), "Layer 1")
			} else {
				w.WriteLwwString(2, zero, "")
			}
			return w.WriteLwwBool(3, zero, true)
		})
	}
	block(4, func(w *rms.DataWriter) error {
		w.WriteId(1, id(0, 1))
		w.WriteId(2, id(0, 13))
		w.WriteId(3, zero)
		w.WriteId(4, zero)
		w.WriteTaggedInt(5, 0)
		return w.WriteSubblock(6, func(w *rms.DataWriter) error { w.WriteUint8(2); return w.WriteId(2, id(0, 11)) })
	})
	if _, e := parseNativeScene(buf.Bytes()); e != nil {
		return nil, fmt.Errorf("new scene validation: %w", e)
	}
	return buf.Bytes(), nil
}

func sceneSwapUUID(b []byte) {
	b[0], b[3] = b[3], b[0]
	b[1], b[2] = b[2], b[1]
	b[4], b[5] = b[5], b[4]
	b[6], b[7] = b[7], b[6]
}

// NotebookContent produces firmware-format v2 content matching the author
// identity stored in a native page. It deliberately uses device CRDT indices,
// not wall-clock timestamps or invented sortable timestamps.
func NotebookContent(pageID string, pageBytes []byte) (map[string]any, error) {
	if pageID == "" {
		return nil, fmt.Errorf("page ID cannot be empty")
	}
	s, e := parseNativeScene(pageBytes)
	if e != nil {
		return nil, e
	}
	author := ""
	for _, block := range s.blocks {
		if b, ok := block.(*rms.AuthorIdsBlock); ok {
			if raw := b.Authors[1]; len(raw) == 16 {
				uuid := append([]byte{}, raw...)
				sceneSwapUUID(uuid)
				h := hex.EncodeToString(uuid)
				author = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
			}
		}
	}
	if author == "" {
		return nil, fmt.Errorf("page has no native author ID 1")
	}
	return map[string]any{
		"cPages":          map[string]any{"lastOpened": map[string]any{"timestamp": "1:1", "value": pageID}, "original": map[string]any{"timestamp": "0:0", "value": -1}, "pages": []any{map[string]any{"id": pageID, "idx": map[string]any{"timestamp": "1:2", "value": "ba"}, "template": map[string]any{"timestamp": "1:2", "value": "Blank"}}}, "uuids": []any{map[string]any{"first": author, "second": 1}}},
		"coverPageNumber": -1, "customZoomCenterX": 0, "customZoomCenterY": int(s.h) / 2, "customZoomOrientation": "portrait", "customZoomPageHeight": int(s.h), "customZoomPageWidth": int(s.w), "customZoomScale": 1, "documentMetadata": map[string]any{}, "extraMetadata": map[string]any{}, "fileType": "notebook", "fontName": "", "formatVersion": 2, "lineHeight": -1, "margins": 125, "orientation": "portrait", "pageCount": 1, "pageTags": []any{}, "sizeInBytes": "0", "tags": []any{}, "textAlignment": "justify", "textScale": 1, "zoomMode": "bestFit",
	}, nil
}

func strokeNumber(v any, defaultValue float64) (float64, error) {
	if v == nil {
		return defaultValue, nil
	}
	var n float64
	switch v := v.(type) {
	case json.Number:
		var e error
		n, e = v.Float64()
		if e != nil {
			return 0, e
		}
	case float64:
		n = v
	case float32:
		n = float64(v)
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	default:
		return 0, fmt.Errorf("invalid numeric stroke value %T", v)
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("stroke numbers must be finite")
	}
	return n, nil
}
func strokeEnum(v any, defaultValue int, aliases map[string]int) (int, error) {
	if v == nil {
		return defaultValue, nil
	}
	if s, ok := v.(string); ok {
		key := strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(strings.TrimSpace(s)))
		n, ok := aliases[key]
		if !ok {
			return 0, fmt.Errorf("unknown stroke enum %q", s)
		}
		return n, nil
	}
	n, e := strokeNumber(v, float64(defaultValue))
	if e != nil || n != math.Trunc(n) {
		return 0, fmt.Errorf("invalid stroke enum")
	}
	return int(n), nil
}
func AuthorPage(original []byte, strokes []map[string]any) ([]byte, error) {
	if len(strokes) == 0 {
		return nil, fmt.Errorf("no strokes provided")
	}
	s, e := parseNativeScene(original)
	if e != nil {
		return nil, e
	}
	if s.legacy {
		return nil, fmt.Errorf("stroke authoring requires a v6 page")
	}
	var layer rms.CrdtId
	found := false
	for i := len(s.blocks) - 1; i >= 0; i-- {
		if v, ok := s.blocks[i].(*rms.SceneLineItemBlock); ok {
			layer = v.ParentID
			found = true
			break
		}
	}
	if !found {
		for i := len(s.blocks) - 1; i >= 0; i-- {
			if v, ok := s.blocks[i].(*rms.SceneGroupItemBlock); ok {
				layer = v.Item.Value
				found = true
				break
			}
		}
	}
	if !found {
		for i := len(s.blocks) - 1; i >= 0; i-- {
			if v, ok := s.blocks[i].(*rms.TreeNodeBlock); ok && v.Group.Label != "" {
				layer = v.Group.NodeID
				found = true
				break
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("no drawable layer found; open this page once on the device")
	}
	maxID := uint64(0)
	authorID := uint8(0)
	for _, block := range s.blocks {
		if b, ok := block.(*rms.AuthorIdsBlock); ok {
			for id := range b.Authors {
				if id > uint16(authorID) && id <= 255 {
					authorID = uint8(id)
				}
			}
		}
	}
	if authorID == 0 {
		return nil, fmt.Errorf("scene has no usable CRDT author identity")
	}
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		if !v.IsValid() {
			return
		}
		if v.Type() == reflect.TypeOf(rms.CrdtId{}) && v.CanInterface() {
			id := v.Interface().(rms.CrdtId)
			if id.Part2 > maxID {
				maxID = id.Part2
			}
			return
		}
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice, reflect.Array:
			if v.Type().Elem().Kind() != reflect.Uint8 {
				for i := 0; i < v.Len(); i++ {
					walk(v.Index(i))
				}
			}
		}
	}
	for _, b := range s.blocks {
		walk(reflect.ValueOf(b))
	}
	for _, t := range s.texts {
		for _, c := range t.chars {
			if c.id.Part2 > maxID {
				maxID = c.id.Part2
			}
		}
	}
	tools := map[string]int{"paintbrush": 12, "pencil": 14, "ballpoint": 15, "marker": 16, "fineliner": 17, "highlighter": 18, "eraser": 6, "mechanical_pencil": 13, "calligraphy": 21, "shader": 23, "brush": 12, "pen": 17, "mechanicalpencil": 13, "caligraphy": 21, "eraser_area": 8}
	for name, ids := range map[string][2]int{"paintbrush": {0, 12}, "pencil": {1, 14}, "ballpoint": {2, 15}, "marker": {3, 16}, "fineliner": {4, 17}, "highlighter": {5, 18}, "mechanical_pencil": {7, 13}} {
		tools[name+"_1"] = ids[0]
		tools[name+"_2"] = ids[1]
	}
	colors := map[string]int{"black": 0, "gray": 1, "grey": 1, "white": 2, "yellow": 3, "green": 4, "pink": 5, "blue": 6, "red": 7, "gray_overlap": 8, "highlight": 9, "green_2": 10, "cyan": 11, "magenta": 12, "yellow_2": 13}
	var appended bytes.Buffer
	w := rms.NewDataWriter(&appended)
	for i, st := range strokes {
		tool, e := strokeEnum(st["tool"], 17, tools)
		if e != nil {
			return nil, e
		}
		if _, ok := map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 12: true, 13: true, 14: true, 15: true, 16: true, 17: true, 18: true, 21: true, 23: true}[tool]; !ok {
			return nil, fmt.Errorf("unsupported pen tool %d", tool)
		}
		col, e := strokeEnum(st["color"], 0, colors)
		if e != nil || col < 0 || col > 13 {
			return nil, fmt.Errorf("invalid pen color")
		}
		baseWidth, baseScale := 16.0, 2.0
		if tool == 5 || tool == 18 {
			baseWidth, baseScale = 30, 2
		} else if tool == 2 || tool == 15 {
			baseWidth = 12
		} else if tool == 21 {
			baseWidth, baseScale = 9, 2.2
		}
		width, e := strokeNumber(st["width"], baseWidth)
		if e != nil {
			return nil, e
		}
		scale, e := strokeNumber(st["thickness_scale"], baseScale)
		if e != nil || scale <= 0 {
			return nil, fmt.Errorf("invalid thickness scale")
		}
		line := &rms.Line{Tool: rms.PenTool(tool), Color: rms.PenColor(col), ThicknessScale: scale}
		value := reflect.ValueOf(st["points"])
		if !value.IsValid() || (value.Kind() != reflect.Slice && value.Kind() != reflect.Array) || value.Len() == 0 {
			return nil, fmt.Errorf("a stroke needs at least one point")
		}
		for j := 0; j < value.Len(); j++ {
			rp := value.Index(j)
			if rp.Kind() == reflect.Interface {
				rp = rp.Elem()
			}
			if (rp.Kind() != reflect.Slice && rp.Kind() != reflect.Array) || rp.Len() < 2 {
				return nil, fmt.Errorf("point needs x and y")
			}
			nx, e := strokeNumber(rp.Index(0).Interface(), 0)
			if e != nil {
				return nil, e
			}
			ny, e := strokeNumber(rp.Index(1).Interface(), 0)
			if e != nil {
				return nil, e
			}
			pressure := 90.0
			if rp.Len() > 2 {
				pressure, e = strokeNumber(rp.Index(2).Interface(), 90)
				if e != nil {
					return nil, e
				}
			}
			line.Points = append(line.Points, rms.Point{X: float32((math.Max(0, math.Min(1, nx)) - .5) * s.w), Y: float32(math.Max(0, math.Min(1, ny)) * s.h), Width: uint16(math.Max(0, math.Min(65535, width))), Pressure: uint8(math.Max(0, math.Min(255, pressure)))})
		}
		if maxID > math.MaxUint64-uint64(i)-1 {
			return nil, fmt.Errorf("CRDT counter overflow")
		}
		block := &rms.SceneLineItemBlock{ParentID: layer, Item: rms.Item[*rms.Line]{ItemID: rms.CrdtId{Part1: authorID, Part2: maxID + uint64(i) + 1}, Value: line}}
		if e = w.WriteSceneLineItemBlock(block, 2); e != nil {
			return nil, e
		}
	}
	result := make([]byte, 0, len(original)+appended.Len())
	result = append(result, original...)
	result = append(result, appended.Bytes()...)
	return result, nil
}
