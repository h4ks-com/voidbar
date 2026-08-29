package rest

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"sort"
)

// The protobuf settings endpoints store an opaque serialized blob per user
// and kind. No protobuf schema is linked into voidbar; instead the wire
// format itself carries the merge semantics Discord documents for these
// endpoints: a PATCH replaces whole top-level fields ("unchanged top-level
// fields can be omitted"), so splitting the stored and incoming blobs into
// top-level (field number, record) pairs is enough to merge faithfully.
// Bonus: field numbers and lengths are varint-prefixed, making the splitter
// a ~40-line reader with no dependencies.

// protoField is one top-level protobuf record: the tag varint plus its
// payload, unmodified.
type protoField struct {
	num int
	raw []byte
}

// splitProtoFields splits a serialized message into its top-level records.
// Groups (wire types 3/4, long deprecated) and malformed input yield an
// error; callers then fall back to storing the blob whole.
func splitProtoFields(b []byte) ([]protoField, error) {
	var out []protoField
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 || tag>>3 == 0 {
			return nil, fmt.Errorf("bad tag")
		}
		rec := append([]byte(nil), b[:n]...) // tag bytes, extended below
		b = b[n:]
		switch tag & 7 {
		case 0: // varint
			_, m := binary.Uvarint(b)
			if m <= 0 {
				return nil, fmt.Errorf("bad varint")
			}
			rec = append(rec, b[:m]...)
			b = b[m:]
		case 1: // fixed64
			if len(b) < 8 {
				return nil, fmt.Errorf("short fixed64")
			}
			rec = append(rec, b[:8]...)
			b = b[8:]
		case 2: // length-delimited
			l, m := binary.Uvarint(b)
			if m <= 0 || uint64(len(b)-m) < l {
				return nil, fmt.Errorf("bad length")
			}
			rec = append(rec, b[:m+int(l)]...)
			b = b[m+int(l):]
		case 5: // fixed32
			if len(b) < 4 {
				return nil, fmt.Errorf("short fixed32")
			}
			rec = append(rec, b[:4]...)
			b = b[4:]
		default:
			return nil, fmt.Errorf("unsupported wire type %d", tag&7)
		}
		out = append(out, protoField{num: int(tag >> 3), raw: rec})
	}
	return out, nil
}

// mergeProtoFields merges patch into stored by top-level field number:
// fields present in the patch replace the stored ones entirely (Discord's
// documented contract), absent fields survive. Output field order is
// ascending for determinism.
func mergeProtoFields(stored, patch []byte) ([]byte, error) {
	base, err := splitProtoFields(stored)
	if err != nil {
		return nil, err
	}
	inc, err := splitProtoFields(patch)
	if err != nil {
		return nil, err
	}
	fields := make(map[int][]byte, len(base)+len(inc))
	for _, f := range base {
		fields[f.num] = f.raw
	}
	for _, f := range inc {
		fields[f.num] = f.raw
	}
	nums := make([]int, 0, len(fields))
	for n := range fields {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var out []byte
	for _, n := range nums {
		out = append(out, fields[n]...)
	}
	return out, nil
}

// settingsProtoPatchBody is the JSON body of the settings-proto PATCH.
type settingsProtoPatchBody struct {
	Settings           string `json:"settings"`
	RequiredDataVersion *int  `json:"required_data_version"`
}

// decodeSettingsProto decodes and size-checks the base64 settings blob of
// a PATCH (Discord caps it at 5MB).
func decodeSettingsProto(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, nil
	}
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(blob) > 5<<20 {
		return nil, fmt.Errorf("settings proto too large")
	}
	return blob, nil
}
