#!/usr/bin/env bash
# Run every numbered dev/smoke/ script and print an aggregate
# summary. Each script self-skips (exit 77) when its required env
# vars aren't set, so a partial-credentials run doesn't count those
# as failures.
#
# Exit code: 0 when every script that actually ran passed; 1 if any
# failed.

set -uo pipefail

dir="$(cd "$(dirname "$0")" && pwd)"
total=0
passed=0
skipped=0
failed=()

for script in "${dir}"/[0-9]*.sh; do
    name="$(basename "${script}")"
    total=$((total + 1))
    echo
    echo "============================================"
    echo "  ${name}"
    echo "============================================"
    bash "${script}"
    rc=$?
    case ${rc} in
        0)
            passed=$((passed + 1))
            ;;
        77)
            skipped=$((skipped + 1))
            ;;
        *)
            failed+=("${name}")
            ;;
    esac
done

echo
echo "============================================"
echo "  summary"
echo "============================================"
printf 'total:   %d\n' "${total}"
printf 'passed:  %d\n' "${passed}"
printf 'skipped: %d\n' "${skipped}"
printf 'failed:  %d' "${#failed[@]}"
if (( ${#failed[@]} > 0 )); then
    printf '  (%s)' "${failed[*]}"
fi
echo

if (( ${#failed[@]} > 0 )); then
    exit 1
fi
