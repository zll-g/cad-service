package main

// 反射访问 go-cad 实体字段。
//
// go-cad 的实体类型定义在 internal/entity 包里，外部无法 import，也就无法做类型断言；
// 但 cad.Document 是类型别名，字段（ModelSpace/Blocks/Attribs/LayerColors/InternalObjs）
// 和实体结构体的导出字段都可以通过反射读取。这里集中封装，字段缺失时返回零值，
// go-cad 升级改名时只会"少返回信息"而不会 panic。
//
// 依赖的字段（go-cad v0.0.0-20261001090957-ddd91eea9618）：
//   公共    Handle Layer Owner TypeName
//   INSERT  Position Scale Rotation(弧度) BlockHeader Attribs
//   TEXT    Text Insertion Height Rotation HAlign VAlign AlignPt
//   MTEXT   Text Insertion TextHeight XAxisDir
//   DIMENSION  UserText ActualMeasurement TextMidpoint AnonymousBlock DimFlag
//              Point10 Point13 Point14 Point15 HasPoint15 DimRotation

import (
	"math"
	"reflect"
	"strings"
)

func rval(e any) reflect.Value {
	v := reflect.ValueOf(e)
	for v.IsValid() && (v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

func field(e any, name string) reflect.Value {
	v := rval(e)
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return v.FieldByName(name)
}

func fFloat(e any, name string) float64 {
	f := field(e, name)
	if f.IsValid() && (f.Kind() == reflect.Float64 || f.Kind() == reflect.Float32) {
		return f.Float()
	}
	return 0
}

func fString(e any, name string) string {
	f := field(e, name)
	if f.IsValid() && f.Kind() == reflect.String {
		return f.String()
	}
	return ""
}

func fUint(e any, name string) uint64 {
	f := field(e, name)
	if !f.IsValid() {
		return 0
	}
	switch f.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return f.Uint()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(f.Int())
	}
	return 0
}

func fBool(e any, name string) bool {
	f := field(e, name)
	return f.IsValid() && f.Kind() == reflect.Bool && f.Bool()
}

func fUints(e any, name string) []uint64 {
	f := field(e, name)
	if !f.IsValid() || f.Kind() != reflect.Slice {
		return nil
	}
	if s, ok := f.Interface().([]uint64); ok {
		return s
	}
	return nil
}

// pointOf 读取 Point2/Point3（或指向它们的指针）字段的 X、Y。
func pointOf(v reflect.Value) (x, y float64, ok bool) {
	for v.IsValid() && v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return 0, 0, false
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return 0, 0, false
	}
	fx, fy := v.FieldByName("X"), v.FieldByName("Y")
	if !fx.IsValid() || !fy.IsValid() || fx.Kind() != reflect.Float64 {
		return 0, 0, false
	}
	return fx.Float(), fy.Float(), true
}

func fPoint(e any, name string) (x, y float64, ok bool) { return pointOf(field(e, name)) }

// kindOf 实体 Go 类型名，如 "EntInsert"、"EntText"。
func kindOf(e any) string {
	v := rval(e)
	if !v.IsValid() {
		return ""
	}
	return v.Type().Name()
}

// entTypeName 对外展示的实体类型名：优先 go-cad 记录的 DXF 类型名（DWG 路径有），
// DXF 路径为空时由 Go 类型名推导（EntLwPolyline → LWPOLYLINE）。
func entTypeName(e any) string {
	if t := fString(e, "TypeName"); t != "" {
		return t
	}
	return strings.ToUpper(strings.TrimPrefix(kindOf(e), "Ent"))
}

// textAnchor 与 go-cad Tessellator 相同的文字锚点：TEXT/ATTRIB 非默认对齐时取对齐点。
func textAnchor(e any) (x, y float64) {
	x, y, _ = fPoint(e, "Insertion")
	if fUint(e, "HAlign") != 0 || fUint(e, "VAlign") != 0 {
		if ax, ay, ok := fPoint(e, "AlignPt"); ok {
			x, y = ax, ay
		}
	}
	return
}

// ---------------- 仿射变换（与 go-cad internal/drawing.Xform 公式逐项一致） ----------------

type xform struct{ sx, sy, cos, sin, tx, ty float64 }

func identity() xform { return xform{sx: 1, sy: 1, cos: 1} }

func (t xform) apply(x, y float64) (float64, float64) {
	return x*t.sx*t.cos - y*t.sy*t.sin + t.tx, x*t.sx*t.sin + y*t.sy*t.cos + t.ty
}

func (t xform) compose(c xform) xform {
	return xform{
		sx: t.sx * c.sx, sy: t.sy * c.sy,
		cos: t.cos*c.cos - t.sin*c.sin,
		sin: t.sin*c.cos + t.cos*c.sin,
		tx:  t.tx + t.cos*t.sx*c.tx - t.sin*t.sy*c.ty,
		ty:  t.ty + t.sin*t.sx*c.tx + t.cos*t.sy*c.ty,
	}
}

// rotation 变换后的旋转角（度，0~360）。
func (t xform) rotationDeg() float64 {
	d := math.Atan2(t.sin, t.cos) * 180 / math.Pi
	if d < 0 {
		d += 360
	}
	return d
}

func insertXform(e any) xform {
	px, py, _ := fPoint(e, "Position")
	sx, sy, ok := fPoint(e, "Scale")
	if !ok {
		sx, sy = 1, 1
	}
	r := fFloat(e, "Rotation")
	return xform{sx: sx, sy: sy, cos: math.Cos(r), sin: math.Sin(r), tx: px, ty: py}
}

func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// plausible 与 go-cad drawing.Plausible 同口径（|x|,|y| < 1e7）：过滤解码错位产生的
// 天文数字坐标。注意：带 3 度带号的大地坐标（X≈39,500,000）也会被 go-cad 过滤掉。
func plausible(x, y float64) bool {
	return finite(x, y) && math.Abs(x) < 1e7 && math.Abs(y) < 1e7
}
