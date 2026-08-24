package lambdalabs

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
)

// vendorShapes is testdata/openapi-shapes.json: the parts of Lambda's
// published OpenAPI document this adapter decodes, recorded rather than
// fetched so the suite stays offline.
type vendorShapes struct {
	InstanceStatusEnum          []string `json:"instance_status_enum"`
	InstanceProperties          []string `json:"instance_properties"`
	InstanceTypeProperties      []string `json:"instance_type_properties"`
	InstanceTypeSpecsProperties []string `json:"instance_type_specs_properties"`
	RegionProperties            []string `json:"region_properties"`
	SSHKeyProperties            []string `json:"ssh_key_properties"`
}

func loadVendorShapes(t *testing.T) vendorShapes {
	t.Helper()
	raw, err := os.ReadFile("testdata/openapi-shapes.json")
	if err != nil {
		t.Fatalf("read recorded vendor shapes: %v", err)
	}
	var out vendorShapes
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode recorded vendor shapes: %v", err)
	}
	return out
}

// jsonTags returns the json field names a struct decodes, walking embedded
// anonymous structs the way encoding/json does not need to here.
func jsonTags(t *testing.T, v any) []string {
	t.Helper()
	rt := reflect.TypeOf(v)
	var out []string
	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		for j := range len(tag) {
			if tag[j] == ',' {
				tag = tag[:j]
				break
			}
		}
		out = append(out, tag)
	}
	slices.Sort(out)
	return out
}

// A json tag that names a field Lambda does not publish decodes to the zero
// value on every response, and a zero value is indistinguishable from a real
// one. Absent reads as false, as empty, as no address, and the adapter acts
// on it. That failure is silent in unit tests built from the adapter's own
// fixtures, which is exactly how it survives to a rental.
func TestDecodedFieldsExistInTheVendorsSchema(t *testing.T) {
	shapes := loadVendorShapes(t)
	cases := []struct {
		what     string
		decoded  []string
		declared []string
	}{
		{"instance", jsonTags(t, apiInstance{}), shapes.InstanceProperties},
		{"instance_type", jsonTags(t, instanceTypeBlock{}), shapes.InstanceTypeProperties},
		{"instance_type.specs", jsonTags(t, instanceTypeBlock{}.Specs), shapes.InstanceTypeSpecsProperties},
		{"ssh_key", jsonTags(t, apiSSHKey{}), shapes.SSHKeyProperties},
	}
	for _, c := range cases {
		for _, field := range c.decoded {
			if !slices.Contains(c.declared, field) {
				t.Errorf("%s: the adapter decodes %q, which Lambda does not publish; it will always be zero", c.what, field)
			}
		}
	}
}

// The status field is the one the deploy path branches on, so an unhandled
// value is not cosmetic: it falls through to the PENDING default and the
// caller waits out a deadline. `preempted` did exactly that until issue 427.
func TestMapLambdaStateCoversTheVendorsEnum(t *testing.T) {
	shapes := loadVendorShapes(t)
	if len(shapes.InstanceStatusEnum) == 0 {
		t.Fatal("recorded status enum is empty")
	}
	handled := map[string]bool{}
	for k := range lambdaStates {
		handled[k] = true
	}
	for _, status := range shapes.InstanceStatusEnum {
		if !handled[status] {
			t.Errorf("Lambda publishes status %q and mapLambdaState has no case for it; it would map to PENDING and the caller would wait on a state it cannot leave", status)
		}
	}
	for status := range handled {
		if !slices.Contains(shapes.InstanceStatusEnum, status) {
			t.Errorf("mapLambdaState handles %q, which Lambda no longer publishes; the recorded schema or the mapping has drifted", status)
		}
	}
}

// Region carries a description the adapter promotes into metadata, and the
// per-card VRAM operators keep expecting is not in the schema at all. Pinned
// so a refresh of the recording surfaces either change.
func TestVendorPublishesNoPerCardVRAM(t *testing.T) {
	shapes := loadVendorShapes(t)
	for _, field := range shapes.InstanceTypeSpecsProperties {
		if field == "gpu_memory_gib" || field == "vram_gb" {
			t.Errorf("Lambda now publishes %q; the catalog's hand-transcribed VRAM can stop being the source", field)
		}
	}
	if !slices.Contains(shapes.RegionProperties, "description") {
		t.Error("region.description is gone; lambdaMetadata promotes it")
	}
}
