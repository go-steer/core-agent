// Copyright 2026 Google LLC
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

package autonomous

import "context"

// Pre-#492 names. The package name already says "autonomous", so
// autonomous.RunAutonomous / autonomous.AutonomousHandle stuttered;
// the primary names are now Run / Start / Resume / Handle / Option.
// These aliases and wrappers keep existing callers building for one
// release.

// AutonomousHandle is the pre-#492 name for Handle.
//
// Deprecated: use Handle.
type AutonomousHandle = Handle

// AutonomousOption is the pre-#492 name for Option.
//
// Deprecated: use Option.
type AutonomousOption = Option

// RunAutonomous is the pre-#492 name for Run.
//
// Deprecated: use Run.
func RunAutonomous(ctx context.Context, build BuildFunc, goal string, opts ...Option) (RunResult, error) {
	return Run(ctx, build, goal, opts...)
}

// StartAutonomous is the pre-#492 name for Start.
//
// Deprecated: use Start.
func StartAutonomous(ctx context.Context, build BuildFunc, goal string, opts ...Option) (*Handle, error) {
	return Start(ctx, build, goal, opts...)
}

// ResumeAutonomous is the pre-#492 name for Resume.
//
// Deprecated: use Resume.
func ResumeAutonomous(ctx context.Context, build ResumeBuildFunc, ref SessionRef, opts ...Option) (RunResult, error) {
	return Resume(ctx, build, ref, opts...)
}
