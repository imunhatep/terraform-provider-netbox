package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fbreckle/go-netbox/netbox/client/virtualization"
	"github.com/fbreckle/go-netbox/netbox/models"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/stretchr/testify/require"
)

// fakeCustomFields is the custom field definitions the fake Netbox serves:
// the SUS group as it exists on our Netbox, plus one field of every other type.
var fakeCustomFields = map[string]string{
	"team":                   "select",
	"ansible_groups":         "text",
	"sus_can_restart":        "boolean",
	"sus_can_update":         "boolean",
	"sus_restart_type":       "select",
	"sus_snapshot":           "text",
	"update_parallelism":     "integer",
	"ratio":                  "decimal",
	"zones":                  "multiselect",
	"settings":               "json",
	"owner_device":           "object",
	"peer_devices":           "multiobject",
	"installed":              "date",
	"documentation":          "url",
	"notes":                  "longtext",
	"maintenance_window_end": "datetime",
}

// fakeNetbox serves the custom field definitions, two per page so that paging
// is exercised, and a virtual machine update endpoint that validates custom
// field values the way Netbox 4.5 does (extras.models.CustomField.validate).
type fakeNetbox struct {
	*httptest.Server
	definitionFetches atomic.Int32
	// lastUpdate is the decoded body of the last virtual machine update.
	lastUpdate map[string]interface{}
}

func newFakeNetbox(t *testing.T) *fakeNetbox {
	t.Helper()
	f := &fakeNetbox{}

	names := make([]string, 0, len(fakeCustomFields))
	for name := range fakeCustomFields {
		names = append(names, name)
	}

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/extras/custom-fields/":
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			if offset == 0 {
				f.definitionFetches.Add(1)
			}
			end := min(offset+2, len(names))
			results := []map[string]interface{}{}
			for _, name := range names[offset:end] {
				results = append(results, map[string]interface{}{
					"id":   len(results) + offset + 1,
					"name": name,
					"type": map[string]interface{}{"value": fakeCustomFields[name], "label": fakeCustomFields[name]},
				})
			}
			var next interface{}
			if end < len(names) {
				next = fmt.Sprintf("%s/api/extras/custom-fields/?limit=2&offset=%d", f.URL, end)
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"count": len(names), "next": next, "previous": nil, "results": results,
			})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/virtualization/virtual-machines/"):
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.lastUpdate = body
			if cf, ok := body["custom_fields"].(map[string]interface{}); ok {
				for name, value := range cf {
					if msg := netboxValidateCustomField(fakeCustomFields[name], value); msg != "" {
						w.WriteHeader(http.StatusBadRequest)
						json.NewEncoder(w).Encode(map[string]interface{}{
							"__all__": []string{fmt.Sprintf("Invalid value for custom field '%s': %s", name, msg)},
						})
						return
					}
				}
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"id": 8238, "name": body["name"]})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// netboxValidateCustomField mirrors the checks Netbox 4.5 applies to a custom
// field value, returning its error message, or "" when Netbox accepts it.
func netboxValidateCustomField(fieldType string, value interface{}) string {
	if value == nil || value == "" {
		return ""
	}
	switch fieldType {
	case "boolean":
		switch value {
		case true, false, float64(0), float64(1):
			return ""
		}
		return "Value must be true or false."
	case "integer":
		if n, ok := value.(float64); ok && n == float64(int64(n)) {
			return ""
		}
		return "Value must be an integer."
	case "decimal":
		if _, ok := value.(float64); ok {
			return ""
		}
		if s, ok := value.(string); ok {
			if _, err := strconv.ParseFloat(s, 64); err == nil {
				return ""
			}
		}
		return "Value must be a decimal."
	case "multiselect", "multiobject":
		if _, ok := value.([]interface{}); ok {
			return ""
		}
		return "Value must be a list."
	case "object":
		if _, ok := value.(float64); ok {
			return ""
		}
		return "Value must be an object ID."
	case "text", "longtext", "select", "url", "date", "datetime":
		if _, ok := value.(string); ok {
			return ""
		}
		return "Value must be a string."
	}
	return ""
}

func (f *fakeNetbox) state(t *testing.T) *providerState {
	t.Helper()
	cfg := Config{APIToken: "test-token", ServerURL: f.URL}
	c, err := cfg.Client()
	require.NoError(t, err)
	return &providerState{NetBoxAPI: c}
}

// updateVirtualMachine sends custom fields to the fake Netbox through the same
// client call resourceNetboxVirtualMachineUpdate makes.
func (f *fakeNetbox) updateVirtualMachine(api *providerState, customFields interface{}) error {
	name := "rix3-bitbucket1-u1"
	data := models.WritableVirtualMachineWithConfigContext{Name: &name, CustomFields: customFields}
	params := virtualization.NewVirtualizationVirtualMachinesUpdateParams().WithID(8238).WithData(&data)
	_, err := api.Virtualization.VirtualizationVirtualMachinesUpdate(params, nil)
	return err
}

// virtualMachineData returns the ResourceData of a virtual machine update: the
// prior state holds the custom fields as the read path stores them, and the
// configuration is what Terraform core hands the provider (an ignore_changes
// entry carries its prior state value).
func virtualMachineData(t *testing.T, prior, config map[string]string) *schema.ResourceData {
	t.Helper()
	sm := schema.InternalMap(resourceNetboxVirtualMachine().Schema)

	attributes := map[string]string{"id": "8238", "name": "rix3-bitbucket1-u1", "cluster_id": "817"}
	attributes["custom_fields.%"] = strconv.Itoa(len(prior))
	for k, v := range prior {
		attributes["custom_fields."+k] = v
	}
	state := &terraform.InstanceState{ID: "8238", Attributes: attributes}

	cf := map[string]interface{}{}
	for k, v := range config {
		cf[k] = v
	}
	raw := map[string]interface{}{"name": "rix3-bitbucket1-u1", "cluster_id": 817, customFieldsKey: cf}

	diff, err := sm.Diff(context.Background(), state, terraform.NewResourceConfigRaw(raw), nil, nil, true)
	require.NoError(t, err)
	d, err := sm.Data(state, diff)
	require.NoError(t, err)
	return d
}

// The SUS custom fields of rix3-bitbucket1-u1 as the read path stores them.
var susState = map[string]string{
	"Environment":      "DEVELOPMENT",
	"ansible_groups":   "proxmox_vms,virtual",
	"team":             "Stockholm SRE",
	"sus_can_restart":  "false",
	"sus_can_update":   "true",
	"sus_restart_type": "Restart",
	"sus_snapshot":     "26Q4",
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestCustomFieldsUntypedUpdateIsRejected pins the fake Netbox to the failure
// seen in CI: resending the string the read path stored for a boolean field.
func TestCustomFieldsUntypedUpdateIsRejected(t *testing.T) {
	f := newFakeNetbox(t)
	err := f.updateVirtualMachine(f.state(t), map[string]interface{}{"sus_can_update": "true"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Invalid value for custom field 'sus_can_update': Value must be true or false.")
}

// TestCustomFieldsUnrelatedUpdateLeavesThemOut covers the CI failure itself: a
// tag-only change on a VM whose state holds SUS booleans must not send
// custom_fields at all.
func TestCustomFieldsUnrelatedUpdateLeavesThemOut(t *testing.T) {
	f := newFakeNetbox(t)
	api := f.state(t)

	d := virtualMachineData(t, susState, susState)
	customFields, err := customFieldsPayload(api, d)
	require.NoError(t, err)
	require.Nil(t, customFields)

	require.NoError(t, f.updateVirtualMachine(api, customFields))
	require.NotContains(t, f.lastUpdate, "custom_fields")
	require.Zero(t, f.definitionFetches.Load(), "nothing to type, so no definitions are fetched")
}

// TestCustomFieldsChangedUpdateIsTyped covers a custom field change on the same
// VM: the SUS booleans ride along from ignore_changes and must reach Netbox as
// booleans.
func TestCustomFieldsChangedUpdateIsTyped(t *testing.T) {
	f := newFakeNetbox(t)
	api := f.state(t)

	config := copyMap(susState)
	config["ansible_groups"] = "proxmox_vms,virtual,bitbucket"
	d := virtualMachineData(t, susState, config)

	customFields, err := customFieldsPayload(api, d)
	require.NoError(t, err)
	require.NoError(t, f.updateVirtualMachine(api, customFields))

	sent := f.lastUpdate["custom_fields"].(map[string]interface{})
	require.Equal(t, true, sent["sus_can_update"])
	require.Equal(t, false, sent["sus_can_restart"])
	require.Equal(t, "proxmox_vms,virtual,bitbucket", sent["ansible_groups"])
	require.Equal(t, "Restart", sent["sus_restart_type"])
	// Environment is not defined on the fake Netbox: passed through untouched.
	require.Equal(t, "DEVELOPMENT", sent["Environment"])
}

// TestCustomFieldsCreateIsTyped covers create, where every configured custom
// field counts as changed.
func TestCustomFieldsCreateIsTyped(t *testing.T) {
	f := newFakeNetbox(t)
	api := f.state(t)

	d := schema.TestResourceDataRaw(t, resourceNetboxVirtualMachine().Schema, map[string]interface{}{
		"name": "vm01",
		customFieldsKey: map[string]interface{}{
			"team":               "Stockholm SRE",
			"sus_can_update":     "true",
			"update_parallelism": "3",
		},
	})
	customFields, err := customFieldsPayload(api, d)
	require.NoError(t, err)
	require.NoError(t, f.updateVirtualMachine(api, customFields))

	sent := f.lastUpdate["custom_fields"].(map[string]interface{})
	require.Equal(t, true, sent["sus_can_update"])
	require.Equal(t, float64(3), sent["update_parallelism"])
	require.Equal(t, "Stockholm SRE", sent["team"])
}

// TestCustomFieldsEveryTypeRoundTrips sends back, through typing, every value
// the read path stores for a value Netbox returned, and expects Netbox to take
// all of them.
func TestCustomFieldsEveryTypeRoundTrips(t *testing.T) {
	f := newFakeNetbox(t)
	api := f.state(t)

	// As Netbox returns them (JSON numbers decode to float64).
	fromNetbox := map[string]interface{}{
		"team":                   "Stockholm SRE",
		"sus_can_update":         true,
		"sus_can_restart":        false,
		"update_parallelism":     float64(1000000),
		"ratio":                  1.5,
		"zones":                  []interface{}{"a", "b"},
		"settings":               map[string]interface{}{"k": "v"},
		"owner_device":           map[string]interface{}{"id": float64(42), "display": "rix3-sw1", "url": "https://netbox/api/dcim/devices/42/"},
		"peer_devices":           []interface{}{map[string]interface{}{"id": float64(7)}, map[string]interface{}{"id": float64(8)}},
		"installed":              "2026-10-14",
		"documentation":          "https://example.com",
		"notes":                  "line1\nline2",
		"maintenance_window_end": "2026-10-14T12:00:00Z",
	}

	stored := getCustomFields(fromNetbox)
	require.Equal(t, "1000000", stored["update_parallelism"], "large integers must not be stored in exponent notation")

	customFields, err := api.typedCustomFields(stored)
	require.NoError(t, err)
	require.NoError(t, f.updateVirtualMachine(api, customFields))

	sent := f.lastUpdate["custom_fields"].(map[string]interface{})
	require.Equal(t, true, sent["sus_can_update"])
	require.Equal(t, false, sent["sus_can_restart"])
	require.Equal(t, float64(1000000), sent["update_parallelism"])
	require.Equal(t, 1.5, sent["ratio"])
	require.Equal(t, []interface{}{"a", "b"}, sent["zones"])
	require.Equal(t, map[string]interface{}{"k": "v"}, sent["settings"])
	require.Equal(t, float64(42), sent["owner_device"])
	require.Equal(t, []interface{}{float64(7), float64(8)}, sent["peer_devices"])
	require.Equal(t, "2026-10-14", sent["installed"])
}

func TestTypedCustomFieldValue(t *testing.T) {
	cases := []struct {
		fieldType string
		in        string
		want      interface{}
		wantErr   string
	}{
		{"integer", "42", int64(42), ""},
		{"integer", "-7", int64(-7), ""},
		{"integer", "1.5", nil, `"1.5" is not an integer`},
		{"integer", "1e+06", nil, `"1e+06" is not an integer`},
		{"decimal", "1.50", 1.5, ""},
		{"decimal", "3", float64(3), ""},
		{"decimal", "NaN", nil, "is not a number"},
		{"decimal", "abc", nil, "is not a number"},
		{"boolean", "true", true, ""},
		{"boolean", "false", false, ""},
		{"boolean", "1", true, ""},
		{"boolean", "0", false, ""},
		{"boolean", "yes", nil, "is not true or false"},
		{"multiselect", `["a","b"]`, []interface{}{"a", "b"}, ""},
		{"multiselect", "a", nil, "is not a JSON list"},
		{"json", `{"a":1}`, map[string]interface{}{"a": float64(1)}, ""},
		{"json", `[1,2]`, []interface{}{float64(1), float64(2)}, ""},
		{"json", "plain text", "plain text", ""},
		{"object", "42", int64(42), ""},
		{"object", `{"id":42,"display":"x"}`, int64(42), ""},
		{"object", `{"display":"x"}`, nil, "neither an object ID nor an object with an id"},
		{"multiobject", "[1,2]", []int64{1, 2}, ""},
		{"multiobject", `[{"id":1},{"id":2}]`, []int64{1, 2}, ""},
		{"multiobject", `["x"]`, nil, "neither an object ID nor an object with an id"},
		{"multiobject", "1", nil, "is not a JSON list"},
		{"text", "42", "42", ""},
		{"longtext", "true", "true", ""},
		{"select", "Restart", "Restart", ""},
		{"url", "https://example.com", "https://example.com", ""},
		{"date", "2026-10-14", "2026-10-14", ""},
		{"datetime", "2026-10-14T12:00:00Z", "2026-10-14T12:00:00Z", ""},
		// A field Netbox does not know: left for Netbox to report.
		{"", "anything", "anything", ""},
	}
	for _, tt := range cases {
		t.Run(tt.fieldType+"/"+tt.in, func(t *testing.T) {
			got, err := typedCustomFieldValue("f", tt.fieldType, tt.in)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.ErrorContains(t, err, `custom field "f"`)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestTypedCustomFieldValueEmptyString: an empty string goes out unchanged for
// every type, as it did before typing, because Netbox accepts it for all of
// them and the read path treats it as unset.
func TestTypedCustomFieldValueEmptyString(t *testing.T) {
	for _, fieldType := range []string{"integer", "decimal", "boolean", "multiselect", "json", "object", "multiobject", "text", "select", ""} {
		got, err := typedCustomFieldValue("f", fieldType, "")
		require.NoError(t, err, fieldType)
		require.Equal(t, "", got, fieldType)
	}
}

func TestTypedCustomFieldsNonStringValuesPassThrough(t *testing.T) {
	f := newFakeNetbox(t)
	got, err := f.state(t).typedCustomFields(map[string]interface{}{"sus_can_update": true, "update_parallelism": int64(2)})
	require.NoError(t, err)
	require.Equal(t, map[string]interface{}{"sus_can_update": true, "update_parallelism": int64(2)}, got)
}

func TestTypedCustomFieldsEmpty(t *testing.T) {
	api := &providerState{}
	for _, cf := range []interface{}{nil, map[string]interface{}{}, map[string]interface{}(nil)} {
		got, err := api.typedCustomFields(cf)
		require.NoError(t, err)
		// An untyped nil, so that omitempty leaves custom_fields out.
		require.True(t, got == nil, "%#v", got)
	}
}

func TestTypedCustomFieldsErrorNamesField(t *testing.T) {
	f := newFakeNetbox(t)
	_, err := f.state(t).typedCustomFields(map[string]interface{}{"update_parallelism": "many"})
	require.EqualError(t, err, `custom field "update_parallelism" is an integer field, "many" is not an integer`)
}

// TestCustomFieldTypesAreCached: the definitions are fetched once per provider
// run, and again only when a field the cache does not know shows up.
func TestCustomFieldTypesAreCached(t *testing.T) {
	f := newFakeNetbox(t)
	api := f.state(t)

	for i := 0; i < 3; i++ {
		_, err := api.typedCustomFields(map[string]interface{}{"sus_can_update": "true"})
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, f.definitionFetches.Load())

	types, err := api.customFieldTypes([]string{"team"})
	require.NoError(t, err)
	require.Equal(t, fakeCustomFields, types, "every page of definitions is read")
	require.EqualValues(t, 1, f.definitionFetches.Load())

	_, err = api.typedCustomFields(map[string]interface{}{"created_in_this_apply": "x"})
	require.NoError(t, err)
	require.EqualValues(t, 2, f.definitionFetches.Load(), "an unknown field refreshes the definitions")
}

func TestCustomFieldTypesFetchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"detail":"You do not have permission to perform this action."}`))
	}))
	defer ts.Close()

	cfg := Config{APIToken: "test-token", ServerURL: ts.URL}
	c, err := cfg.Client()
	require.NoError(t, err)

	_, err = (&providerState{NetBoxAPI: c}).typedCustomFields(map[string]interface{}{"sus_can_update": "true"})
	require.ErrorContains(t, err, "listing custom field definitions")
}

func TestCustomFieldValueToStringNumbers(t *testing.T) {
	require.Equal(t, "1000000", customFieldValueToString(float64(1000000)))
	require.Equal(t, "1.5", customFieldValueToString(1.5))
	require.Equal(t, "-3", customFieldValueToString(float64(-3)))
	require.Equal(t, "true", customFieldValueToString(true))
}
