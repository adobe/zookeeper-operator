/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package zk

import (
	v1 "k8s.io/api/core/v1"
)

// SyncConfigMap synchronizes a configmap with an updated spec and validates it
func SyncConfigMap(curr *v1.ConfigMap, next *v1.ConfigMap) {
	curr.Data = next.Data
	curr.BinaryData = next.BinaryData
}
