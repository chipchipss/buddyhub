// Cursor AgentService 的 protobuf 编解码与 Connect 帧（同包文件，
// 移植自 magpie-plugins packages/cursor/index.mjs L861-1030，MIT）。
//
// 只实现对话协议用到的子集：varint / length-delimited / fixed64 / fixed32
// 四种 wire type，以及 google.protobuf.Value 的 JSON↔pb 互转。
// 不依赖任何 protobuf 库——fork 也是手写的（~90 行），语义照搬。
package cursor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// WireType protobuf wire type（低 3 位）。
const (
	WireVarint  = 0
	WireFixed64 = 1
	WireBytes   = 2
	WireFixed32 = 5
)

// Field 解析出的一个字段（fork 的 {num, wire, n, data}）。
// Varint 的值在 N；Bytes 的负载在 Data；Fixed64/Fixed32 的值在 N
// （fork 对 wire 1 读 uint64 LE 存 N，wire 5 读 uint32 LE 存 N）。
type Field struct {
	Num  int
	Wire int
	N    uint64
	Data []byte
}

// Str 取字符串字段（找不到返回 ""，fork pbStr）。
func Str(fields []Field, num int) string {
	f := Find(fields, num)
	if f == nil {
		return ""
	}
	return string(f.Data)
}

// Num 取 varint/fixed 字段的值（找不到返回 0，fork pbNum）。
func Num(fields []Field, num int) uint64 {
	f := Find(fields, num)
	if f == nil {
		return 0
	}
	return f.N
}

// Find 取第一个命中的字段。
func Find(fields []Field, num int) *Field {
	for i := range fields {
		if fields[i].Num == num {
			return &fields[i]
		}
	}
	return nil
}

// ── 解析（fork fields L900-951）────────────────────────────────────────

// Fields 解析一条消息的字段。畸形尾部**丢到即止**（fork 的纪律：
// 部分字段可用就用，不整个报错）。
func Fields(b []byte) []Field {
	var out []Field
	i := 0
	uv := func() int64 {
		var v uint64
		var m uint64 = 1
		for i < len(b) {
			c := b[i]
			i++
			v += uint64(c&0x7f) * m
			if c < 0x80 {
				return int64(v)
			}
			m *= 128
		}
		return -1
	}
	for i < len(b) {
		key := uv()
		if key < 0 {
			break
		}
		f := Field{Num: int(key / 8), Wire: int(key % 8)}
		switch f.Wire {
		case WireVarint:
			v := uv()
			if v < 0 {
				return out
			}
			f.N = uint64(v)
		case WireFixed64:
			if i+8 > len(b) {
				return out
			}
			f.Data = b[i : i+8]
			f.N = binary.LittleEndian.Uint64(b[i:])
			i += 8
		case WireBytes:
			l := uv()
			if l < 0 || i+int(l) > len(b) {
				return out
			}
			f.Data = b[i : i+int(l)]
			i += int(l)
		case WireFixed32:
			if i+4 > len(b) {
				return out
			}
			f.N = uint64(binary.LittleEndian.Uint32(b[i:]))
			i += 4
		default:
			return out // 未知 wire type：丢尾部
		}
		out = append(out, f)
	}
	return out
}

// ── 构造（fork PB L861-897）───────────────────────────────────────────

// Builder 逐段拼装消息（fork 的 pb() 链式对象）。
type Builder struct {
	buf []byte
}

// PB 建一个空构造器。
func PB() *Builder { return &Builder{} }

// Bytes 追加原始字节段。
func (b *Builder) Bytes(raw []byte) *Builder {
	b.buf = append(b.buf, raw...)
	return b
}

// Raw 取已拼装的字节。
func (b *Builder) Raw() []byte { return b.buf }

// UV 拼一个 varint（裸值，无 tag）。
func (b *Builder) UV(v uint64) *Builder {
	for v >= 0x80 {
		b.buf = append(b.buf, byte(v%128)|0x80)
		v /= 128
	}
	b.buf = append(b.buf, byte(v))
	return b
}

// Tag 拼一个字段头（num<<3 | wire）。
func (b *Builder) Tag(num, wire int) *Builder {
	return b.UV(uint64(num)*8 + uint64(wire))
}

// Varint 字段：num + varint 值。
func (b *Builder) Varint(num int, v uint64) *Builder {
	return b.Tag(num, WireVarint).UV(v)
}

// Str 字段：num + 长度 + UTF-8 字节。
func (b *Builder) Str(num int, s string) *Builder {
	return b.BytesField(num, []byte(s))
}

// BytesField 字段：num + 长度 + 负载。
func (b *Builder) BytesField(num int, v []byte) *Builder {
	return b.Tag(num, WireBytes).UV(uint64(len(v))).Bytes(v)
}

// Sub 字段：num + 长度 + 子构造器（fork 的 bytes(num, PB)）。
func (b *Builder) Sub(num int, sub *Builder) *Builder {
	return b.BytesField(num, sub.Raw())
}

// Double 字段：num + 8 字节小端 double。
func (b *Builder) Double(num int, v float64) *Builder {
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], math.Float64bits(v))
	return b.Tag(num, WireFixed64).Bytes(raw[:])
}

// Fixed32 字段：num + 4 字节小端 uint32。
func (b *Builder) Fixed32(num int, v uint32) *Builder {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], v)
	return b.Tag(num, WireFixed32).Bytes(raw[:])
}

// ── Connect 帧（fork frame/frames L1005-1030）─────────────────────────

// MaxFrame 单帧上限（fork 的 64<<20）：超出按畸形流处理，防内存爆。
const MaxFrame = 64 << 20

// EncodeFrame 把一条消息包成 Connect 帧：1 字节 flags + 4 字节大端长度 + 负载。
// flags 的 bit0 = compressed（gzip），bit1 = end（流结束）。
func EncodeFrame(msg []byte, flags byte) []byte {
	head := make([]byte, 5)
	head[0] = flags
	binary.BigEndian.PutUint32(head[1:], uint32(len(msg)))
	return append(head, msg...)
}

// DecodeFrame 从缓冲里取一帧。
// 返回 (帧负载, flags, 剩余缓冲, ok)。数据不足时 ok=false（等更多字节）。
func DecodeFrame(buf []byte) (data []byte, flags byte, rest []byte, ok bool) {
	if len(buf) < 5 {
		return nil, 0, buf, false
	}
	n := binary.BigEndian.Uint32(buf[1:5])
	if n > MaxFrame {
		return nil, 0, buf, false // 调用方据此判畸形流
	}
	if uint32(len(buf)-5) < n {
		return nil, 0, buf, false
	}
	return buf[5 : 5+n], buf[0], buf[5+n:], true
}

// ErrMalformedStream 帧长度超上限或流提前断（fork 抛的两类）。
var ErrMalformedStream = errors.New("cursorpb: malformed connect stream")

// ── google.protobuf.Value（fork pbValue L957-1002）────────────────────

// Value 把任意 JSON 值编成 google.protobuf.Value 的 pb 字节。
// 形态：null=varint(1,0)；number=double(2,v)；string=str(3,s)；
// bool=varint(4,0|1)；array=bytes(6, 每元素 bytes(1,Value))；
// object=bytes(5, 每项 {str(1,k), bytes(2,Value)})。
func Value(v any) (*Builder, error) {
	switch t := v.(type) {
	case nil:
		return PB().Varint(1, 0), nil
	case bool:
		bit := uint64(0)
		if t {
			bit = 1
		}
		return PB().Varint(4, bit), nil
	case string:
		return PB().Str(3, t), nil
	case float64:
		return PB().Double(2, t), nil
	case int:
		return PB().Double(2, float64(t)), nil
	case int64:
		return PB().Double(2, float64(t)), nil
	case []any:
		list := PB()
		for _, e := range t {
			ev, err := Value(e)
			if err != nil {
				return nil, err
			}
			list.Sub(1, ev)
		}
		return PB().Sub(6, list), nil
	case map[string]any:
		// fork L970：st.bytes(1, pb().str(1,k).bytes(2,val)) 后整体
		// bytes(5, st)——**一个** field 5，里面每项是独立的 field 1，
		// 每项内部 {str(1,key), bytes(2,Value)}。
		// key 排序（fork 的 Object.keys(v).sort()）：既复刻字节形状，
		// 也让 Go map 无序性不影响输出确定性。
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		st := PB()
		for _, k := range keys {
			vv, err := Value(t[k])
			if err != nil {
				return nil, err
			}
			st.Sub(1, PB().Str(1, k).Sub(2, vv))
		}
		return PB().Sub(5, st), nil
	default:
		return nil, fmt.Errorf("cursorpb: unsupported value type %T", v)
	}
}

// AnyValue 把 google.protobuf.Value 的 pb 字节解回 Go 值（fork pbAny）。
func AnyValue(data []byte) (any, error) {
	fs := Fields(data)
	switch {
	case Find(fs, 1) != nil: // null
		return nil, nil
	case Find(fs, 2) != nil: // number
		f := Find(fs, 2)
		if len(f.Data) < 8 {
			return nil, fmt.Errorf("cursorpb: short double")
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(f.Data)), nil
	case Find(fs, 3) != nil: // string
		return Str(fs, 3), nil
	case Find(fs, 4) != nil: // bool
		return Num(fs, 4) != 0, nil
	case Find(fs, 5) != nil: // struct：**单个** field 5，负载里每项是
		// repeated field 1，每项内部 {str(1,key), bytes(2,Value)}
		// （fork pbAny case 5：fields(f.data) 里找 num===1）。
		out := map[string]any{}
		for _, item := range Fields(Find(fs, 5).Data) {
			if item.Num != 1 {
				continue
			}
			kv := Fields(item.Data)
			vf := Find(kv, 2)
			if vf == nil {
				out[Str(kv, 1)] = nil
				continue
			}
			v, err := AnyValue(vf.Data)
			if err != nil {
				return nil, err
			}
			out[Str(kv, 1)] = v
		}
		return out, nil
	case Find(fs, 6) != nil: // list：单个 field 6，负载里每个元素是
		// repeated field 1（fork：pb().bytes(6, list) 而 list 逐项
		// bytes(1, pbValue(e))）。
		var out []any
		f := Find(fs, 6)
		for _, item := range Fields(f.Data) {
			if item.Num != 1 {
				continue
			}
			v, err := AnyValue(item.Data)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, nil
}
