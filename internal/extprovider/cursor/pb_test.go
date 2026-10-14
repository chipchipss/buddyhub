package cursor

// pb.go 测试：编解码对 fork 语义的逐字还原 + Connect 帧往返 + Value 互转。
import (
	"bytes"
	"encoding/binary"
	"testing"
)

// varint 往返：含多字节（>127）与 fork 相同的 LEB128 小端序。
func TestPBVarintRoundtrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, 16384, 1 << 40} {
		b := PB().Varint(1, v).Raw()
		fs := Fields(b)
		if len(fs) != 1 || fs[0].Num != 1 || fs[0].N != v {
			t.Fatalf("varint %d 往返 = %+v", v, fs)
		}
	}
	// 字节形状钉死：300 = 0b1_0010_1100 → AC 02（fork 的 %128|0x80 算法）。
	raw := PB().UV(300).Raw()
	if !bytes.Equal(raw, []byte{0xAC, 0x02}) {
		t.Fatalf("varint 300 字节 = % x, want AC 02", raw)
	}
}

// 字符串/bytes 字段往返（含 UTF-8 与空串）。
func TestPBStringRoundtrip(t *testing.T) {
	b := PB().Str(1, "qoder-协议").Str(2, "").Raw()
	fs := Fields(b)
	if Str(fs, 1) != "qoder-协议" {
		t.Fatalf("str = %q", Str(fs, 1))
	}
	if f := Find(fs, 2); f == nil || len(f.Data) != 0 {
		t.Fatalf("空串字段 = %+v", f)
	}
}

// 嵌套子消息（fork 的 bytes(num, PB)）往返。
func TestPBSubMessage(t *testing.T) {
	inner := PB().Str(1, "id-123").Varint(2, 7)
	outer := PB().Sub(1, inner).Str(2, "x").Raw()
	fs := Fields(outer)
	sub := Find(fs, 1)
	if sub == nil {
		t.Fatal("子消息字段缺失")
	}
	subFs := Fields(sub.Data)
	if Str(subFs, 1) != "id-123" || Num(subFs, 2) != 7 {
		t.Fatalf("子消息内容错: %+v", subFs)
	}
	if Str(fs, 2) != "x" {
		t.Fatal("兄弟字段丢失")
	}
}

// double / fixed32 往返。
func TestPBFixedWidth(t *testing.T) {
	b := PB().Double(1, 3.5).Fixed32(2, 0xDEADBEEF).Raw()
	fs := Fields(b)
	f1 := Find(fs, 1)
	if f1 == nil || len(f1.Data) != 8 || binary.LittleEndian.Uint64(f1.Data) != 0x400C000000000000 {
		t.Fatalf("double 3.5 字节错: % x", f1.Data)
	}
	if Num(fs, 2) != 0xDEADBEEF {
		t.Fatalf("fixed32 = %x", Num(fs, 2))
	}
}

// 畸形输入：截断的 varint / 超长 bytes / 未知 wire type 都只丢尾部
// （fork fields 的纪律：部分可用就用）。
func TestPBFieldsMalformed(t *testing.T) {
	// 完整字段 + 截断的第二个 key。
	good := PB().Str(1, "ok").Raw()
	bad := append(append([]byte{}, good...), 0x0A) // bytes 头后无长度
	fs := Fields(bad)
	if len(fs) != 1 || Str(fs, 1) != "ok" {
		t.Fatalf("畸形尾应保留已解析字段: %+v", fs)
	}
	// 空输入。
	if len(Fields(nil)) != 0 {
		t.Fatal("nil 输入应无字段")
	}
	// 未知 wire type（3/4 不是合法 wire type）：立即停。
	raw := PB().Str(1, "a").Raw()
	raw = append(raw, 0x18) // num=3, wire=0 合法；改用 wire 3：num=3,wire=3 → 27
	raw = append(raw[:len(raw)-1], 0x1B)
	if fs := Fields(raw); len(fs) != 1 {
		t.Fatalf("未知 wire type 后应停: %+v", fs)
	}
}

// Connect 帧：编码→解码往返，flags 透传，多帧粘包正确切分。
func TestConnectFrameRoundtrip(t *testing.T) {
	msg := []byte(`{"hello":"cursor"}`)
	f := EncodeFrame(msg, 0)
	if len(f) != 5+len(msg) {
		t.Fatalf("帧长 = %d", len(f))
	}
	if f[0] != 0 {
		t.Fatal("flags 丢失")
	}
	if binary.BigEndian.Uint32(f[1:5]) != uint32(len(msg)) {
		t.Fatal("长度头不是大端 uint32")
	}
	data, flags, rest, ok := DecodeFrame(f)
	if !ok || flags != 0 || !bytes.Equal(data, msg) || len(rest) != 0 {
		t.Fatalf("单帧往返错: ok=%v flags=%d rest=%d", ok, flags, len(rest))
	}

	// 两帧粘在一起：第一次取第一帧，第二次取第二帧。
	two := append(EncodeFrame([]byte("first"), 0), EncodeFrame([]byte("second"), 2)...)
	d1, _, r1, ok1 := DecodeFrame(two)
	d2, fl2, r2, ok2 := DecodeFrame(r1)
	if !ok1 || !ok2 || string(d1) != "first" || string(d2) != "second" {
		t.Fatalf("粘包切分错: %q %q", d1, d2)
	}
	if fl2 != 2 || len(r2) != 0 {
		t.Fatalf("end flags 或残余错: %d %d", fl2, len(r2))
	}
}

// 帧解析的"数据不足"：半个头、头齐负载不足，都要 ok=false 且缓冲原样。
func TestConnectFrameIncomplete(t *testing.T) {
	msg := []byte("payload")
	full := EncodeFrame(msg, 0)
	if _, _, rest, ok := DecodeFrame(full[:3]); ok || len(rest) != 3 {
		t.Fatal("半个头应等更多字节")
	}
	if _, _, rest, ok := DecodeFrame(full[:8]); ok || len(rest) != 8 {
		t.Fatal("负载不足应等更多字节")
	}
}

// 超限帧：长度头 >64MB 判畸形（fork 的 n > 64<<20）。
func TestConnectFrameOversize(t *testing.T) {
	head := make([]byte, 5)
	binary.BigEndian.PutUint32(head[1:], MaxFrame+1)
	if _, _, _, ok := DecodeFrame(head); ok {
		t.Fatal("超限帧应判失败（防内存爆）")
	}
}

// 前向兼容（coder 的观察）：上游新增**未知字段号**但用标准 wire type 时，
// 解析必须按 wire type 跳过、后面的已知字段照常读到——手写 pb 最容易
// 在这里崩，必须钉死而不是等连真实上游才暴露。
func TestPBUnknownFieldSkippedByWiretype(t *testing.T) {
	// 手工拼一条消息：field 99(varint) + field 100(bytes) + 已知 field 1(str)
	// + field 101(fixed64) + 已知 field 2(varint)。
	b := PB().Tag(99, WireVarint).UV(4242).
		Tag(100, WireBytes).UV(3).Bytes([]byte("xxx")).
		Str(1, "known").
		Tag(101, WireFixed64).Bytes(make([]byte, 8)).
		Varint(2, 7).
		Raw()
	fs := Fields(b)
	if Str(fs, 1) != "known" {
		t.Fatalf("已知 str 字段被未知字段带崩: %q", Str(fs, 1))
	}
	if Num(fs, 2) != 7 {
		t.Fatalf("已知 varint 字段丢失（未知 fixed64 之后的字段读不到）: %+v", fs)
	}
	// 未知字段本身应完整保留在结果里（供需要时透传），不是被丢弃。
	if Find(fs, 99) == nil || Find(fs, 100) == nil || Find(fs, 101) == nil {
		t.Fatalf("未知字段应解析保留: %+v", fs)
	}
}

// 未知 **wire type**（3/4，protobuf 已废弃的 group）：与 fork 同一口径
// ——丢尾部保已解析（fork fields L945-946 `default: return out`）。
// 我们逐字移植 fork，上游是 proto3（不使用 group），此处把"与 fork 一致"
// 钉死：若将来要支持 group 跳过，这条会提醒改了语义。
func TestPBUnknownWireTypeMatchesFork(t *testing.T) {
	good := PB().Str(1, "ok").Raw()
	// 追加一个 wire type 3（group start）的头：解析应在此停住。
	bad := append(append([]byte{}, good...), 0x0B) // num=1, wire=3
	fs := Fields(bad)
	if len(fs) != 1 || Str(fs, 1) != "ok" {
		t.Fatalf("未知 wire type 应保留已解析字段: %+v", fs)
	}
	// group 型 wire 4（end）同理。
	bad2 := append(append([]byte{}, good...), 0x0C) // num=1, wire=4
	if len(Fields(bad2)) != 1 {
		t.Fatal("wire=4 应同样停住")
	}
}

// google.protobuf.Value 互转：五种 JSON 形态全覆盖（fork pbValue/pbAny）。
func TestPBValueRoundtrip(t *testing.T) {
	cases := []any{
		nil,
		"text",
		3.25,
		true,
		[]any{"a", 1.0, nil},
		map[string]any{"single": "value"},
	}
	for _, in := range cases {
		b, err := Value(in)
		if err != nil {
			t.Fatalf("Value(%v): %v", in, err)
		}
		out, err := AnyValue(b.Raw())
		if err != nil {
			t.Fatalf("AnyValue(%v): %v", in, err)
		}
		// 逐形态比对（map 单键规避 Go map 无序问题）。
		switch want := in.(type) {
		case nil:
			if out != nil {
				t.Fatalf("null 往返 = %v", out)
			}
		case []any:
			got, ok := out.([]any)
			if !ok || len(got) != len(want) {
				t.Fatalf("list 往返 = %v", out)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("list[%d] = %v, want %v", i, got[i], want[i])
				}
			}
		case map[string]any:
			got, ok := out.(map[string]any)
			if !ok || got["single"] != want["single"] {
				t.Fatalf("struct 往返 = %v", out)
			}
		default:
			if out != in {
				t.Fatalf("%T 往返 = %v, want %v", in, out, in)
			}
		}
	}
}

// 不支持的形态：Value 报错（不透支成静默零值）。
func TestPBValueUnsupported(t *testing.T) {
	if _, err := Value(make(chan int)); err == nil {
		t.Fatal("不支持的类型应报错")
	}
}

// 嵌套 Value：list 里套 struct（工具参数 args 的真实形态）。
func TestPBValueNested(t *testing.T) {
	in := map[string]any{
		"namespace": "magpie",
		"toolName":  "read_file",
		"arguments": map[string]any{"path": "main.go"},
	}
	b, err := Value(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := AnyValue(b.Raw())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("类型 = %T", out)
	}
	args, ok := got["arguments"].(map[string]any)
	if !ok || args["path"] != "main.go" {
		t.Fatalf("嵌套 arguments = %v", got["arguments"])
	}
	if got["toolName"] != "read_file" {
		t.Fatalf("toolName = %v", got["toolName"])
	}
}
