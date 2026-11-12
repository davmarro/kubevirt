/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 */

package storage

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"kubevirt.io/client-go/testutils"
)

func TestStorage(t *testing.T) {
	// Standalone command tests need the template helpers normally set by virtctl.
	cobra.AddTemplateFunc("ProgramName", func() string { return "virtctl" })
	cobra.AddTemplateFunc("prepare", func(s string) string { return strings.ReplaceAll(s, "{{ProgramName}}", "virtctl") })
	testutils.KubeVirtTestSuiteSetup(t)
}
