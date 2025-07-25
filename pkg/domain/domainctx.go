// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package domain

import (
	contextutil "github.com/pingcap/tidb/pkg/util/context"
)

// GetDomain gets domain from context.
// might return nil if the session is a cross keyspace one.
func GetDomain(ctx contextutil.ValueStoreContext) *Domain {
	v, ok := ctx.GetDomain().(*Domain)
	if ok {
		return v
	}
	return nil
}
