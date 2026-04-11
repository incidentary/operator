/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package wireformat

import "encoding/json"

// marshalFlatStringMap marshals a string map as a plain JSON object. Nil maps
// become an empty object so the wire payload never contains a `null` resource.
func marshalFlatStringMap(m map[string]string) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

// unmarshalFlatStringMap hydrates a string map from a JSON object. `null` is
// treated as an empty map.
func unmarshalFlatStringMap(data []byte) (map[string]string, error) {
	if len(data) == 0 || string(data) == "null" {
		return map[string]string{}, nil
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}
