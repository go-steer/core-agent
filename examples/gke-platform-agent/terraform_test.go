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

// Originally derived from go-steer/mast:charts/terraform_test.go.
//
// deploy/terraform is a declarative way to provision the GCP IAM and
// cluster prerequisites for gke-platform-agent alongside scripts/grant-iam.sh
// and the Kustomize tree under deploy/. `terraform test` checks the module
// against itself — that write_scope decides the role set, that one namespace
// reaches both halves — but it cannot see scripts/grant-iam.sh or the
// Kustomize manifests under deploy/, so everything in this file is a claim
// that spans two artifacts and can therefore only rot in one of them.
//
// Two of these guards also pin a property the HCL tests structurally cannot
// assert at all: an assertion in a .tftest.hcl reads values, and "this
// resource is google_project_iam_member and not google_project_iam_binding"
// is not a value — it is the difference between a module that adds a binding
// to somebody else's project and one that takes the project's IAM policy over.

package gkeplatformagent_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	tfRoot        = "deploy/terraform"
	tfRootVars    = tfRoot + "/variables.tf"
	tfWifMain     = tfRoot + "/modules/wif/main.tf"
	tfWifVars     = tfRoot + "/modules/wif/variables.tf"
	tfWifOutputs  = tfRoot + "/modules/wif/outputs.tf"
	tfReleaseMain = tfRoot + "/modules/release/main.tf"
	tfReleaseVars = tfRoot + "/modules/release/variables.tf"
	grantIAMPath  = "scripts/grant-iam.sh"
)

// authoritativeIAM is the set of resources that replace an IAM policy
// instead of adding to it. google_project_iam_binding owns a role across
// every member in the project; google_project_iam_policy owns the whole
// policy. Either one, in a module an operator drops into a project that
// already runs things, deletes bindings this module never created — and
// it does it on the first apply, silently, because Terraform considers
// that convergence rather than damage.
var authoritativeIAM = []string{
	"google_project_iam_binding",
	"google_project_iam_policy",
	"google_service_account_iam_binding",
	"google_service_account_iam_policy",
	"google_folder_iam_policy",
	"google_organization_iam_policy",
}

// secretCreators write a value into Terraform state. The module names
// the daemon's bearer Secrets and creates neither: a token Terraform
// generates or is handed is stored in the state file in plaintext, and a
// state bucket is usually readable by more people than the cluster Secret
// is. This is the list of ways that decision gets quietly reversed by
// somebody adding a convenience.
var secretCreators = []string{
	"random_password",
	"random_string",
	"random_id",
	"kubernetes_secret",
	"kubernetes_secret_v1",
	"google_secret_manager_secret_version",
}

// TestTerraformWifIsAdditivePerMember pins the resource choice that the
// module's own tests cannot see.
//
// modules/wif's narrowing — flip write_scope back to "namespaced" and
// the roles/container.admin binding is gone — is what the module exists
// for, and wif.tftest.hcl asserts it by reading the bound role set off
// the resources. But that only proves the set shrank. That the removed
// key is DESTROYED rather than orphaned, and that destroying it removes
// one member's grant rather than every member's, is a property of which
// resource type was used, and no assertion in HCL can read a resource's
// type. So it is read here, out of the source.
func TestTerraformWifIsAdditivePerMember(t *testing.T) {
	tf := tfReadFile(t, tfWifMain)

	if !strings.Contains(tf, `resource "google_project_iam_member" "`) {
		t.Errorf("%s binds no google_project_iam_member — the narrowing in wif.tftest.hcl only removes one principal's grant because the bindings are per-member", tfWifMain)
	}
	for _, path := range terraformFiles(t) {
		body := tfReadFile(t, path)
		for _, res := range authoritativeIAM {
			if strings.Contains(body, `resource "`+res+`" "`) {
				t.Errorf("%s declares %s: it is authoritative, so an apply deletes every binding in the operator's project that this module did not create", path, res)
			}
		}
	}
}

// TestTerraformCreatesNoSecret pins the other decision that is invisible
// to the module's own tests: release.tftest.hcl asserts that
// required_secrets NAMES both bearer Secrets, which stays true whether or
// not something else in the configuration also creates them.
func TestTerraformCreatesNoSecret(t *testing.T) {
	for _, path := range terraformFiles(t) {
		body := tfReadFile(t, path)
		for _, res := range secretCreators {
			if strings.Contains(body, `resource "`+res+`" "`) {
				t.Errorf("%s declares %s: a token Terraform authors is written to the state file in plaintext, which is a wider audience than the Secret it would populate", path, res)
			}
		}
	}
}

// TestTerraformNamesTheDeployServiceAccount couples the hardcoded KSA and
// default namespace in the Terraform modules to deploy/base and
// deploy/components/gated-apply.
func TestTerraformNamesTheDeployServiceAccount(t *testing.T) {
	var baseNS, baseSA string
	for _, o := range gatedApplyDecode(t, "deploy/base/10-serviceaccount-daemon.yaml") {
		if o.Kind == "ServiceAccount" {
			baseNS, baseSA = o.Metadata.Namespace, o.Metadata.Name
		}
	}
	if baseNS == "" || baseSA == "" {
		t.Fatal("deploy/base/10-serviceaccount-daemon.yaml carries no ServiceAccount with name and namespace")
	}

	for _, path := range []string{tfWifMain, tfReleaseMain} {
		tfSA, ok := quotedAfter(tfReadFile(t, path), "ksa_name")
		if !ok {
			t.Fatalf("%s no longer sets a literal ksa_name", path)
		}
		if tfSA != baseSA {
			t.Errorf("deploy/base creates ServiceAccount %q and %s names %q — the WIF principal or RBAC subject would bind an identity that does not exist", baseSA, path, tfSA)
		}
	}

	objs := gatedApplyDecode(t, filepath.Join(gatedApplyDir, "rolebinding.yaml"))
	if len(objs) != 1 || len(objs[0].Subjects) != 1 {
		t.Fatalf("expected 1 RoleBinding with 1 subject in %s/rolebinding.yaml", gatedApplyDir)
	}
	m := gatedApplySubjectRE.FindStringSubmatch(objs[0].Subjects[0].Name)
	if m == nil {
		t.Fatalf("subject %q is not a Workload Identity username", objs[0].Subjects[0].Name)
	}
	if wifSA, _ := quotedAfter(tfReadFile(t, tfWifMain), "ksa_name"); m[3] != wifSA {
		t.Errorf("gated-apply rolebinding.yaml binds KSA %q while %s names %q", m[3], tfWifMain, wifSA)
	}

	for _, varsPath := range []string{tfRootVars, tfWifVars, tfReleaseVars} {
		ns, ok := hclVariableDefault(tfReadFile(t, varsPath), "namespace")
		if !ok {
			t.Fatalf("%s no longer declares a literal default for variable \"namespace\"", varsPath)
		}
		if ns != baseNS {
			t.Errorf("%s defaults namespace to %q while deploy/base uses %q", varsPath, ns, baseNS)
		}
	}
}

// TestTerraformAndGrantIAMBuildTheSamePrincipal compares the two
// identity strings character for character, with only the four
// substituted values held apart.
//
// A principal that differs from the script's is not a test failure an
// operator would ever see as one: IAM accepts a binding for a principal
// that identifies nobody, so the apply succeeds and every tool call
// comes back Forbidden later, mid-incident, with both artifacts reading
// correctly on their own.
func TestTerraformAndGrantIAMBuildTheSamePrincipal(t *testing.T) {
	script, ok := quotedAfter(tfReadFile(t, grantIAMPath), "ksa_principal() {\n    echo ")
	if !ok {
		t.Fatalf("%s no longer defines ksa_principal() in a shape this test can read", grantIAMPath)
	}
	tf, ok := quotedAfter(tfReadFile(t, tfWifMain), "principal = ")
	if !ok {
		t.Fatalf("%s no longer sets a literal principal", tfWifMain)
	}
	if got, want := tfPrincipalHoles.Replace(tf), tfPrincipalHoles.Replace(script); got != want {
		t.Errorf("the module and %s build different WIF principals, so one of them binds nobody:\n  terraform: %s\n  script:    %s", grantIAMPath, got, want)
	}
}

// tfPrincipalHoles normalizes the two principal templates down to the shape
// they share: the same five path segments around the same four substituted
// values. Everything else — a missing /locations/global, a pool named for
// the project number instead of the project ID — survives the replacement
// and fails the comparison, which is the point.
var tfPrincipalHoles = strings.NewReplacer(
	"${PROJECT_NUMBER}", "<number>",
	"${PROJECT_ID}", "<project>",
	"${DEMO_NS}", "<namespace>",
	"$1", "<ksa>",
	"${local.project_number}", "<number>",
	"${var.project_id}", "<project>",
	"${var.namespace}", "<namespace>",
	"${local.ksa_name}", "<ksa>",
)

// TestTerraformAndGrantIAMGrantTheSameRoles holds the module to
// scripts/grant-iam.sh for both write scopes.
func TestTerraformAndGrantIAMGrantTheSameRoles(t *testing.T) {
	script := tfReadFile(t, grantIAMPath)
	tf := tfReadFile(t, tfWifMain)

	for _, scope := range []string{"namespaced", "cluster-admin"} {
		t.Run(scope, func(t *testing.T) {
			want := grantIAMProjectRoles(t, script, scope)
			got := terraformProjectRoles(t, tf, scope)
			if !slices.Equal(got, want) {
				t.Errorf("write_scope=%s binds %v and %s base set for %s is %v", scope, got, grantIAMPath, scope, want)
			}
		})
	}

	// The fourth binding is not a project role and lives in its own
	// resource, so the loop above cannot see it. Without it the daemon
	// cannot impersonate the node service account and Workload Identity
	// Federation never completes.
	if !strings.Contains(tf, `"roles/iam.serviceAccountUser"`) {
		t.Errorf("%s binds no roles/iam.serviceAccountUser on the node service account, which %s does", tfWifMain, grantIAMPath)
	}
	if !strings.Contains(script, "roles/iam.serviceAccountUser") {
		t.Errorf("%s no longer grants roles/iam.serviceAccountUser on the node service account", grantIAMPath)
	}
}

// TestTerraformAndGrantIAMEnableTheSameAPIs is the same argument one
// layer down: an API the module forgets is an apply that succeeds and a
// daemon that cannot reach Vertex or exchange a token.
func TestTerraformAndGrantIAMEnableTheSameAPIs(t *testing.T) {
	want := grantIAMBaseAPIs(t, tfReadFile(t, grantIAMPath))
	got := hclList(t, tfReadFile(t, tfWifMain), "apis = var.enable_apis ? toset([")

	slices.Sort(want)
	slices.Sort(got)
	if len(want) == 0 {
		t.Fatalf("%s enables no base APIs — this test would pass on anything", grantIAMPath)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the module enables %v and %s enables %v", got, grantIAMPath, want)
	}
}

// TestTerraformReleaseMatchesGatedApplyRBAC couples the per-namespace
// Role and RoleBinding in deploy/terraform/modules/release/main.tf to
// deploy/components/gated-apply/{role.yaml,rolebinding.yaml}.
func TestTerraformReleaseMatchesGatedApplyRBAC(t *testing.T) {
	roles := gatedApplyDecode(t, filepath.Join(gatedApplyDir, "role.yaml"))
	if len(roles) != 1 || len(roles[0].Rules) != 1 {
		t.Fatalf("expected 1 Role with 1 rule in %s/role.yaml", gatedApplyDir)
	}
	wantRule := roles[0].Rules[0]

	tf := tfReadFile(t, tfReleaseMain)
	_, roleBlock, ok := strings.Cut(tf, `resource "kubernetes_role_v1" "remediation"`)
	if !ok {
		t.Fatalf("%s declares no kubernetes_role_v1.remediation resource", tfReleaseMain)
	}
	roleBlock, _, _ = strings.Cut(roleBlock, `resource "kubernetes_role_binding_v1"`)

	gotGroups := hclList(t, roleBlock, "api_groups = [")
	gotResources := hclList(t, roleBlock, "resources  = [")
	gotVerbs := hclList(t, roleBlock, "verbs      = [")

	if !slices.Equal(gotGroups, wantRule.APIGroups) {
		t.Errorf("release module Role api_groups = %v, want %v from %s/role.yaml", gotGroups, wantRule.APIGroups, gatedApplyDir)
	}
	if !slices.Equal(gotResources, wantRule.Resources) {
		t.Errorf("release module Role resources = %v, want %v from %s/role.yaml", gotResources, wantRule.Resources, gatedApplyDir)
	}
	if !slices.Equal(gotVerbs, wantRule.Verbs) {
		t.Errorf("release module Role verbs = %v, want %v from %s/role.yaml", gotVerbs, wantRule.Verbs, gatedApplyDir)
	}

	rbs := gatedApplyDecode(t, filepath.Join(gatedApplyDir, "rolebinding.yaml"))
	if len(rbs) != 1 || len(rbs[0].Subjects) != 1 {
		t.Fatalf("expected 1 RoleBinding with 1 subject in %s/rolebinding.yaml", gatedApplyDir)
	}
	_, rbBlock, ok := strings.Cut(tf, `resource "kubernetes_role_binding_v1" "remediation"`)
	if !ok {
		t.Fatalf("%s declares no kubernetes_role_binding_v1.remediation resource", tfReleaseMain)
	}
	_, subjBlock, ok := strings.Cut(rbBlock, "subject {")
	if !ok {
		t.Fatalf("%s kubernetes_role_binding_v1.remediation has no subject block", tfReleaseMain)
	}
	gotKind, ok := quotedAfter(subjBlock, "kind")
	if !ok || gotKind != rbs[0].Subjects[0].Kind {
		t.Errorf("release module RoleBinding subject kind = %q, want %q from %s/rolebinding.yaml", gotKind, rbs[0].Subjects[0].Kind, gatedApplyDir)
	}

	_, roleRefBlock, ok := strings.Cut(rbBlock, "role_ref {")
	if !ok {
		t.Fatalf("%s kubernetes_role_binding_v1.remediation has no role_ref block", tfReleaseMain)
	}
	gotRefKind, ok := quotedAfter(roleRefBlock, "kind")
	if !ok || gotRefKind != "Role" {
		t.Errorf("release module RoleBinding role_ref kind = %q, want \"Role\"", gotRefKind)
	}

	rbacNameTmpl, ok := quotedAfter(tf, "rbac_name")
	if !ok {
		t.Fatalf("%s no longer sets a literal rbac_name", tfReleaseMain)
	}
	resolvedRBACName := strings.ReplaceAll(rbacNameTmpl, "${var.namespace}", "gke-platform-agent")
	if resolvedRBACName != roles[0].Metadata.Name || resolvedRBACName != rbs[0].Metadata.Name {
		t.Errorf("release module rbac_name under default namespace = %q, want %q (role.yaml) / %q (rolebinding.yaml)", resolvedRBACName, roles[0].Metadata.Name, rbs[0].Metadata.Name)
	}

	wantSubj := strings.NewReplacer(
		"your-project-id", "${var.project_id}",
		"gke-platform-agent", "${var.namespace}",
		"core-agent-daemon", "${local.ksa_name}",
	).Replace(rbs[0].Subjects[0].Name)
	gotReleaseSubj, ok := quotedAfter(tf, "rbac_subject = ")
	if !ok || gotReleaseSubj != wantSubj {
		t.Errorf("%s rbac_subject = %q, want %q (matching %s/rolebinding.yaml)", tfReleaseMain, gotReleaseSubj, wantSubj, gatedApplyDir)
	}
	wifOutBody := tfReadFile(t, tfWifOutputs)
	_, wifSubjBlock, ok := strings.Cut(wifOutBody, `output "rbac_subject"`)
	if !ok {
		t.Fatalf("%s has no output \"rbac_subject\" block", tfWifOutputs)
	}
	gotWifSubj, ok := quotedAfter(wifSubjBlock, "value")
	if !ok || gotWifSubj != wantSubj {
		t.Errorf("%s rbac_subject = %q, want %q (matching %s/rolebinding.yaml)", tfWifOutputs, gotWifSubj, wantSubj, gatedApplyDir)
	}
}

// grantIAMBaseAPIs parses the base `apis=( ... )` array in scripts/grant-iam.sh.
func grantIAMBaseAPIs(t *testing.T, script string) []string {
	t.Helper()
	_, after, ok := strings.Cut(script, "\napis=(")
	if !ok {
		t.Fatalf("%s has no apis=(...) array", grantIAMPath)
	}
	block, _, ok := strings.Cut(after, "\n)")
	if !ok {
		t.Fatalf("%s apis=(...) array is never closed", grantIAMPath)
	}
	var out []string
	for _, line := range strings.Split(block, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		entry := strings.TrimSpace(line)
		if entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// grantIAMProjectRoles parses the base (non-OTEL) project grants for
// core-agent-daemon from scripts/grant-iam.sh and resolves ${CUSTOM_ROLE}
// to the predefined container role for the given write_scope.
func grantIAMProjectRoles(t *testing.T, script, scope string) []string {
	t.Helper()
	_, after, ok := strings.Cut(script, "\ngrants=()")
	if !ok {
		t.Fatalf("%s has no grants=() array", grantIAMPath)
	}
	baseBlock, _, ok := strings.Cut(after, "\nif [[ \"${OTEL}\"")
	if !ok {
		t.Fatalf("%s has no OTEL guard after base grants=()", grantIAMPath)
	}
	sourceRole, ok := quotedAfter(script, "--source=")
	if !ok {
		t.Fatalf("%s no longer copies CUSTOM_ROLE from a --source= predefined role", grantIAMPath)
	}
	containerRole := sourceRole
	if scope == "cluster-admin" {
		containerRole = "roles/container.admin"
	}

	entries := allQuotedAfter(baseBlock, `grants+=("`)
	if len(entries) == 0 {
		t.Fatalf("%s adds no base grants", grantIAMPath)
	}
	var roles []string
	for _, entry := range entries {
		fields := strings.Fields(entry)
		if len(fields) != 2 || fields[1] != "core-agent-daemon" {
			t.Fatalf("unexpected base grant entry %q in %s", entry, grantIAMPath)
		}
		role := fields[0]
		if role == "${CUSTOM_ROLE}" {
			role = containerRole
		}
		roles = append(roles, role)
	}
	slices.Sort(roles)
	return roles
}

// terraformProjectRoles is the local.project_roles list with the
// write_scope ternary resolved for one scope.
func terraformProjectRoles(t *testing.T, tf, scope string) []string {
	t.Helper()
	m := containerRoleTernary.FindStringSubmatch(tf)
	if m == nil {
		t.Fatalf("%s no longer resolves container_role from write_scope in a shape this test can read", tfWifMain)
	}
	container := m[3]
	if scope == m[1] {
		container = m[2]
	}

	roles := hclList(t, tf, "project_roles = [")
	var resolved int
	for i, r := range roles {
		if r == "local.container_role" {
			roles[i] = container
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("%s's project_roles referenced local.container_role %d times; expected exactly one", tfWifMain, resolved)
	}
	slices.Sort(roles)
	return roles
}

// containerRoleTernary matches
// `container_role = var.write_scope == "X" ? "role-if-X" : "role-otherwise"`.
var containerRoleTernary = regexp.MustCompile(
	`container_role\s*=\s*var\.write_scope\s*==\s*"([^"]+)"\s*\?\s*"([^"]+)"\s*:\s*"([^"]+)"`)

// hclList reads the entries of a bracketed list that starts at prefix,
// returning quoted strings verbatim and bare expressions (e.g.
// local.container_role) as their source text, so a caller can resolve
// them.
func hclList(t *testing.T, body, prefix string) []string {
	t.Helper()
	_, after, ok := strings.Cut(body, prefix)
	if !ok {
		t.Fatalf("no list starting %q in the module source", prefix)
	}
	block, _, ok := strings.Cut(after, "]")
	if !ok {
		t.Fatalf("the list starting %q is never closed", prefix)
	}

	var out []string
	for _, line := range strings.Split(block, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		for _, part := range strings.Split(line, ",") {
			entry := strings.TrimSpace(part)
			if entry == "" {
				continue
			}
			out = append(out, strings.Trim(entry, `"`))
		}
	}
	return out
}

// hclVariableDefault extracts the default string of a named variable block.
func hclVariableDefault(body, name string) (string, bool) {
	_, after, ok := strings.Cut(body, `variable "`+name+`"`)
	if !ok {
		return "", false
	}
	block, _, ok := strings.Cut(after, "\n}")
	if !ok {
		return "", false
	}
	return quotedAfter(block, "default")
}

// quotedAfter returns the first double-quoted string following prefix.
func quotedAfter(body, prefix string) (string, bool) {
	_, after, ok := strings.Cut(body, prefix)
	if !ok {
		return "", false
	}
	_, after, ok = strings.Cut(after, `"`)
	if !ok {
		return "", false
	}
	value, _, ok := strings.Cut(after, `"`)
	return value, ok
}

// allQuotedAfter returns every double-quoted string following each
// occurrence of prefix, in source order.
func allQuotedAfter(body, prefix string) []string {
	var out []string
	rest := body
	for {
		_, after, ok := strings.Cut(rest, prefix)
		if !ok {
			return out
		}
		value, tail, ok := strings.Cut(after, `"`)
		if !ok {
			return out
		}
		out = append(out, value)
		rest = tail
	}
}

func tfReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// terraformFiles is every .tf file in the module tree. The scans above
// walk it rather than naming files, because the way an authoritative IAM
// resource or a generated password arrives is a new file.
func terraformFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(tfRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".terraform" {
			return filepath.SkipDir
		}
		if !d.IsDir() && filepath.Ext(path) == ".tf" {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", tfRoot, err)
	}
	if len(out) == 0 {
		t.Fatalf("no .tf files under %s — these scans would pass on an empty tree", tfRoot)
	}
	return out
}
