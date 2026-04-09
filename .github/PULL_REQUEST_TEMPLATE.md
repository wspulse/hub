## Summary

<!-- What does this PR do and why? One or two sentences. -->

## Related issues <!-- optional -->

<!-- Link related issues: "Closes #N" if resolved by this PR, "Relates to org/repo#N" if cross-repo. Remove this section if none. -->

## Changes

<!-- List the concrete changes made. Be specific. -->

## Checklist

### Required

- [ ] `make check` passes (`fmt` → `lint` → `test`)
- [ ] Each commit represents exactly one logical change
- [ ] Commit messages follow the format in `commit-message-instructions.md`
- [ ] No unrelated code reformatting in this PR

### Conditional (list only those that apply)

<!-- Remove items that do not apply to this PR. All remaining items must be checked before merge. -->

- [ ] Bug fix: includes a test that fails before the fix and passes after
- [ ] New public API: includes GoDoc comments
- [ ] Breaking change: major version bump discussed in linked issue
- [ ] Performance-sensitive change: benchmark included (`make bench` before/after)
- [ ] Heart serialization not violated (no session state mutation outside heart goroutine)
