package netbox

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"sync"

	"github.com/fbreckle/go-netbox/netbox/client/extras"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// customFieldTypeCache holds the type of every Netbox custom field, by name.
//
// The custom_fields schema is a map of strings, but Netbox validates each value
// against the field's type: a boolean field only accepts true or false, an
// integer field only a JSON number. Turning the strings back into those types
// needs the field definitions, which are fetched once and shared by every
// resource. The zero value is ready to use.
type customFieldTypeCache struct {
	mu    sync.Mutex
	types map[string]string
}

// customFieldTypes returns the type of every custom field Netbox knows,
// fetching the definitions on first use and again whenever one of names is
// missing, so a custom field created earlier in the same apply is found.
func (s *providerState) customFieldTypes(names []string) (map[string]string, error) {
	c := &s.customFieldTypeCache
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.types != nil {
		missing := false
		for _, name := range names {
			if _, ok := c.types[name]; !ok {
				missing = true
				break
			}
		}
		if !missing {
			return c.types, nil
		}
	}

	types, err := s.fetchCustomFieldTypes()
	if err != nil {
		return nil, err
	}
	c.types = types
	return types, nil
}

func (s *providerState) fetchCustomFieldTypes() (map[string]string, error) {
	types := map[string]string{}
	limit := int64(DefaultPageSize)
	offset := int64(0)
	for {
		params := extras.NewExtrasCustomFieldsListParams().WithLimit(&limit).WithOffset(&offset)
		res, err := s.Extras.ExtrasCustomFieldsList(params, nil)
		if err != nil {
			return nil, fmt.Errorf("listing custom field definitions: %w", err)
		}
		if res.Payload == nil {
			return types, nil
		}
		for _, field := range res.Payload.Results {
			if field == nil || field.Name == nil || field.Type == nil || field.Type.Value == nil {
				continue
			}
			types[*field.Name] = *field.Type.Value
		}
		offset += int64(len(res.Payload.Results))
		if len(res.Payload.Results) == 0 || res.Payload.Next == nil || offset >= *res.Payload.Count {
			return types, nil
		}
	}
}

// typedCustomFields returns cf with every string value converted to the JSON
// type its Netbox field requires. Values of fields Netbox does not know are
// left as they are, so Netbox reports them itself.
func (s *providerState) typedCustomFields(cf interface{}) (interface{}, error) {
	cfm, ok := cf.(map[string]interface{})
	if !ok || len(cfm) == 0 {
		// An untyped nil, so that the request leaves custom_fields out instead
		// of sending a null map.
		return nil, nil
	}

	names := make([]string, 0, len(cfm))
	for name := range cfm {
		names = append(names, name)
	}
	types, err := s.customFieldTypes(names)
	if err != nil {
		return nil, err
	}

	typed := make(map[string]interface{}, len(cfm))
	for name, value := range cfm {
		v, ok := value.(string)
		if !ok {
			typed[name] = value
			continue
		}
		tv, err := typedCustomFieldValue(name, types[name], v)
		if err != nil {
			return nil, err
		}
		typed[name] = tv
	}
	return typed, nil
}

// typedCustomFieldValue converts one value to the JSON type Netbox expects for
// fieldType. It accepts what customFieldValueToString writes to state, so a
// value read from Netbox can be sent back unchanged.
//
// An empty string is sent as it is for every type: Netbox skips validation for
// it and the read path treats it as unset, exactly as before typing existed.
func typedCustomFieldValue(name, fieldType, v string) (interface{}, error) {
	if v == "" {
		return v, nil
	}

	switch fieldType {
	case "integer":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("custom field %q is an integer field, %q is not an integer", name, v)
		}
		return n, nil
	case "decimal":
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, fmt.Errorf("custom field %q is a decimal field, %q is not a number", name, v)
		}
		return f, nil
	case "boolean":
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("custom field %q is a boolean field, %q is not true or false", name, v)
		}
		return b, nil
	case "multiselect":
		var choices []interface{}
		if err := json.Unmarshal([]byte(v), &choices); err != nil {
			return nil, fmt.Errorf(`custom field %q is a multiple selection field, %q is not a JSON list (use jsonencode(["a", "b"]))`, name, v)
		}
		return choices, nil
	case "json":
		var doc interface{}
		if err := json.Unmarshal([]byte(v), &doc); err != nil {
			// Bare text is a valid JSON document once quoted.
			return v, nil
		}
		return doc, nil
	case "object":
		id, ok := customFieldObjectID(json.RawMessage(v))
		if !ok {
			return nil, fmt.Errorf("custom field %q is an object field, %q is neither an object ID nor an object with an id", name, v)
		}
		return id, nil
	case "multiobject":
		var objects []json.RawMessage
		if err := json.Unmarshal([]byte(v), &objects); err != nil {
			return nil, fmt.Errorf("custom field %q is a multiple object field, %q is not a JSON list of object IDs", name, v)
		}
		ids := make([]int64, 0, len(objects))
		for _, object := range objects {
			id, ok := customFieldObjectID(object)
			if !ok {
				return nil, fmt.Errorf("custom field %q is a multiple object field, %s is neither an object ID nor an object with an id", name, object)
			}
			ids = append(ids, id)
		}
		return ids, nil
	default:
		// text, longtext, url, date, datetime and select take the string itself.
		return v, nil
	}
}

// customFieldObjectID returns the ID an object custom field refers to, given
// either the bare ID or the object as Netbox returns it (and the read path
// stores it as JSON), which carries the ID in its id attribute.
func customFieldObjectID(raw json.RawMessage) (int64, bool) {
	var id int64
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, true
	}
	var object struct {
		ID *int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &object); err == nil && object.ID != nil {
		return *object.ID, true
	}
	return 0, false
}

// customFieldsPayload returns the custom_fields to send in a create or update
// request: the configured custom fields, typed for the Netbox API, when the
// plan changes them, and nil (leave them out) otherwise. On create every
// configured custom field counts as a change, so they are all sent.
func customFieldsPayload(api *providerState, d *schema.ResourceData) (interface{}, error) {
	cf := getCustomFieldsForUpdate(d)
	if cf == nil {
		return nil, nil
	}
	return api.typedCustomFields(cf)
}
