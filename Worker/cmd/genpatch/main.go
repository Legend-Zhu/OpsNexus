// Command genpatch regenerates the two identical generated TunnelFrame stubs
// (Worker + OpsGaurdWeb) after changes to proto/opsguard.proto, WITHOUT protoc.
//
// Why: the deploy environment is offline (no protoc, no Go module proxy), yet
// proto changes must still reach both pb packages. protoc reads the raw
// FileDescriptorProto compiled into the CURRENT pb package (which reflects the
// last .proto state), applies the field increments below (kept in sync with
// proto/opsguard.proto by hand), re-marshals it, and rewrites the rawDesc
// literal + struct fields + getters in both pb.go copies. The result is
// semantically identical to a protoc run for the same field set (modulo
// json_name presence, which is equivalent).
//
// Prefer the standard path whenever protoc exists: `bash proto/gen.sh` already
// falls back to this tool when protoc is missing. When you change .proto:
//  1. update the increments slice in this file to match the new fields;
//  2. run `bash proto/gen.sh` (protoc path) or `cd Worker && go run ./cmd/genpatch patch`.
//
// The tool is idempotent: if TunnelFrame already carries the declared fields it
// exits 0 without touching the files (safe to re-run in gen.sh every time).
//
// Usage:
//
//	go run ./cmd/genpatch patch   # patch Worker pb + copy to server pb
//	go run ./cmd/genpatch verify  # descriptor + round-trip check
package main

import (
	"fmt"
	"os"
	"strings"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	workerPB = "internal/grpcapi/pb/opsguard.pb.go"
	serverPB = "../OpsGaurdWeb/server/internal/workerproxy/pb/opsguard.pb.go"
)

// fieldIncrement is one field to append to TunnelFrame. Keep in sync with
// proto/opsguard.proto's TunnelFrame message (append in field-number order).
type fieldIncrement struct {
	name     string
	number   int32
	typ      descriptorpb.FieldDescriptorProto_Type
	jsonName string // same form protoc derives for camelCase json_name
}

// increments lists the fields added to proto/opsguard.proto on top of what the
// compiled pb package already reflects. When you next extend TunnelFrame, add
// your new fields here (append to the end, matching .proto).
var increments = []fieldIncrement{
	{name: "chunk_seq", number: 8, typ: descriptorpb.FieldDescriptorProto_TYPE_INT32, jsonName: "chunkSeq"},
	{name: "chunk_eof", number: 9, typ: descriptorpb.FieldDescriptorProto_TYPE_BOOL, jsonName: "chunkEof"},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: genpatch patch|verify")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "patch":
		doPatch()
	case "verify":
		doVerify()
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", os.Args[1])
		os.Exit(2)
	}
}

// tfFields returns the current TunnelFrame descriptor fields, keyed by name.
func tfFields() map[string]protoreflect.FieldDescriptor {
	fd := (&pb.TunnelFrame{}).ProtoReflect().Descriptor()
	out := make(map[string]protoreflect.FieldDescriptor, fd.Fields().Len())
	for i := 0; i < fd.Fields().Len(); i++ {
		f := fd.Fields().Get(i)
		out[string(f.Name())] = f
	}
	return out
}

// alreadyPatched reports whether every declared increment is already present
// on the compiled TunnelFrame (idempotency guard).
func alreadyPatched() bool {
	fields := tfFields()
	for _, inc := range increments {
		if fields[inc.name] == nil {
			return false
		}
	}
	return true
}

// extendedDescriptor returns the FileDescriptorProto of the current pb package
// with the increments appended to TunnelFrame.
func extendedDescriptor() *descriptorpb.FileDescriptorProto {
	fd := (&pb.TunnelFrame{}).ProtoReflect().Descriptor().ParentFile()
	dpb := protodesc.ToFileDescriptorProto(fd)

	var tf *descriptorpb.DescriptorProto
	for _, m := range dpb.MessageType {
		if m.GetName() == "TunnelFrame" {
			tf = m
			break
		}
	}
	if tf == nil {
		fatal("TunnelFrame not found in descriptor")
	}
	for _, inc := range increments {
		tf.Field = append(tf.Field, &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(inc.name),
			Number:   proto.Int32(inc.number),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     inc.typ.Enum(),
			JsonName: proto.String(inc.jsonName),
		})
	}
	return dpb
}

// protocQuote escapes one descriptor byte the way protoc-gen-go renders string
// literals: printable ASCII stays verbatim, common control characters use
// short escapes, everything else becomes \xNN.
func protocQuote(b byte) string {
	switch b {
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	case '"':
		return `\"`
	case '\\':
		return `\\`
	case '\a':
		return `\a`
	case '\b':
		return `\b`
	case '\f':
		return `\f`
	case '\v':
		return `\v`
	}
	if b >= 0x20 && b <= 0x7e {
		return string(b)
	}
	return fmt.Sprintf(`\x%02x`, b)
}

// quoteRaw renders b as the joined string-literal body of a Go const
// concatenation, wrapped at ~70 chars on byte boundaries (like protoc output):
//
//	const file_..._rawDesc = "" +
//		"<line 1>" +
//		"<line 2>"
func quoteRaw(b []byte) string {
	var lines []string
	var cur strings.Builder
	for _, c := range b {
		s := protocQuote(c)
		if cur.Len()+len(s) > 70 {
			lines = append(lines, cur.String())
			cur.Reset()
		}
		cur.WriteString(s)
	}
	if cur.Len() > 0 || len(lines) == 0 {
		lines = append(lines, cur.String())
	}
	var sb strings.Builder
	sb.WriteString("const file_opsguard_proto_rawDesc = \"\" +")
	for i, ln := range lines {
		sb.WriteString("\n\t\"")
		sb.WriteString(ln)
		sb.WriteString("\"")
		if i < len(lines)-1 {
			sb.WriteString(" +")
		}
	}
	return sb.String()
}

// patchFile rewrites one pb.go: the rawDesc const block, the TunnelFrame
// struct fields and the getter methods.
func patchFile(path, constBlock, structAdd, getterAdd string) {
	src, err := os.ReadFile(path)
	if err != nil {
		fatal("read %s: %v", path, err)
	}
	lines := strings.Split(string(src), "\n")
	// Normalize CRLF so literal-end detection and the final write stay LF.
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}

	// 1. Replace the rawDesc const block.
	start := -1
	for i, ln := range lines {
		if strings.Contains(ln, "file_opsguard_proto_rawDesc = ") {
			start = i
			break
		}
	}
	if start < 0 {
		fatal("rawDesc block not found in %s", path)
	}
	end := start + 1
	for ; end < len(lines); end++ {
		t := strings.TrimRight(lines[end], " \t")
		if strings.HasSuffix(t, `"`) && !strings.HasSuffix(t, `" +`) {
			break
		}
	}
	rest := append([]string{constBlock}, lines[end+1:]...)
	lines = append(lines[:start], rest...)

	// 2. Insert the struct fields after TunnelFrame.Error.
	idx := indexOf(lines, func(ln string) bool {
		return strings.Contains(ln, `protobuf:"bytes,7,opt,name=error,proto3"`)
	})
	if idx < 0 {
		fatal("TunnelFrame.Error struct field not found in %s", path)
	}
	body := append([]string{}, lines[idx+1:]...)
	lines = append(lines[:idx+1], append([]string{structAdd}, body...)...)

	// 3. Insert the getters before TunnelHeader's first getter.
	idx = indexOf(lines, func(ln string) bool {
		return strings.Contains(ln, "func (x *TunnelHeader) GetKey() string {")
	})
	if idx < 0 {
		fatal("TunnelHeader.GetKey not found in %s", path)
	}
	body = append([]string{}, lines[idx:]...)
	lines = append(lines[:idx], append([]string{getterAdd}, body...)...)

	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		fatal("write %s: %v", path, err)
	}
	fmt.Println("patched", path)
}

func doPatch() {
	if alreadyPatched() {
		fmt.Println("pb already patched (fields present); nothing to do")
		return
	}
	dpb := extendedDescriptor()
	raw, err := proto.Marshal(dpb)
	if err != nil {
		fatal("marshal descriptor: %v", err)
	}
	fmt.Printf("new descriptor size: %d bytes\n", len(raw))

	structAdd := "\t// 分片流式（镜像中继）：chunk_seq 从 0 递增；chunk_eof=true 标记末帧。\n" +
		"\tChunkSeq int32 `protobuf:\"varint,8,opt,name=chunk_seq,json=chunkSeq,proto3\" json:\"chunk_seq,omitempty\"`\n" +
		"\tChunkEof bool  `protobuf:\"varint,9,opt,name=chunk_eof,json=chunkEof,proto3\" json:\"chunk_eof,omitempty\"`\n"

	getterAdd := "func (x *TunnelFrame) GetChunkSeq() int32 {\n" +
		"\tif x != nil {\n" +
		"\t\treturn x.ChunkSeq\n" +
		"\t}\n" +
		"\treturn 0\n" +
		"}\n" +
		"\n" +
		"func (x *TunnelFrame) GetChunkEof() bool {\n" +
		"\tif x != nil {\n" +
		"\t\treturn x.ChunkEof\n" +
		"\t}\n" +
		"\treturn false\n" +
		"}\n" +
		"\n"

	patchFile(workerPB, quoteRaw(raw), structAdd, getterAdd)
	if b, err := os.ReadFile(workerPB); err == nil {
		if err := os.WriteFile(serverPB, b, 0o644); err != nil {
			fatal("copy to server pb: %v", err)
		}
		fmt.Println("copied to", serverPB)
	}
}

func doVerify() {
	fd := (&pb.TunnelFrame{}).ProtoReflect().Descriptor()
	for _, name := range []string{"chunk_seq", "chunk_eof"} {
		f := fd.Fields().ByName(protoreflect.Name(name))
		if f == nil {
			fatal("field %s missing from descriptor", name)
		}
		fmt.Printf("field %s: number=%d\n", name, f.Number())
	}

	// Pure-reflection round trip (compiles against both pre- and post-patch pb).
	in := &pb.TunnelFrame{Id: "x", Status: 200, Body: []byte("hello")}
	m := in.ProtoReflect()
	m.Set(fd.Fields().ByName("chunk_seq"), protoreflect.ValueOfInt32(2))
	m.Set(fd.Fields().ByName("chunk_eof"), protoreflect.ValueOfBool(true))
	raw, err := proto.Marshal(in)
	if err != nil {
		fatal("marshal: %v", err)
	}
	out := &pb.TunnelFrame{}
	if err := proto.Unmarshal(raw, out); err != nil {
		fatal("unmarshal: %v", err)
	}
	om := out.ProtoReflect()
	if om.Get(fd.Fields().ByName("chunk_seq")).Int() != 2 ||
		!om.Get(fd.Fields().ByName("chunk_eof")).Bool() ||
		string(out.GetBody()) != "hello" {
		fatal("round-trip mismatch: %+v", out)
	}
	fmt.Println("round-trip OK: chunk_seq=2 chunk_eof=true body=hello")
}

func indexOf(lines []string, f func(string) bool) int {
	for i, ln := range lines {
		if f(strings.TrimLeft(ln, " \t")) {
			return i
		}
	}
	return -1
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genpatch: "+format+"\n", args...)
	os.Exit(1)
}
