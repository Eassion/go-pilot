---
name: code-review
description: Perform thorough Go code reviews with correctness, security, performance, and maintainability checks.
---

# Code Review Skill

Use this workflow for Go repositories. Keep findings concrete, reproducible, and prioritized by risk.

## Review Checklist

### 1. Security (Critical, Go-focused)

Check for:
- [ ] **Injection vulnerabilities**: SQL/command/template injection from untrusted input
- [ ] **Secrets exposure**: Tokens/keys in code, logs, test fixtures
- [ ] **AuthZ/AuthN flaws**: Missing permission checks, bypassable checks
- [ ] **Unsafe deserialization/parsing** of untrusted payloads
- [ ] **Dependency vulnerabilities** via `govulncheck`

```bash
# Vulnerability and secret scan helpers
govulncheck ./...
rg -n "password|secret|token|api[_-]?key|private[_-]?key" .
```

### 2. Correctness

Check for:
- [ ] **Logic errors**: boundary handling, nil checks, index math
- [ ] **Concurrency bugs**: goroutine leaks, races, lock misuse
- [ ] **Resource leaks**: missing `Close()`, hanging contexts/timers
- [ ] **Error handling**: ignored errors, wrapped context loss
- [ ] **API contract drift**: behavior changes not reflected in callers/tests

### 3. Performance

Check for:
- [ ] **Allocation pressure** in hot paths
- [ ] **Excessive locking** or contention
- [ ] **Blocking I/O** in request-critical paths
- [ ] **Inefficient algorithms/data structures**
- [ ] **Missing cancellation/timeouts** around external calls

### 4. Maintainability

Check for:
- [ ] **Naming and package boundaries** are clear and consistent
- [ ] **Complexity**: functions too long, deeply nested branching
- [ ] **Duplication**: repeated logic not extracted
- [ ] **Dead code**: unreachable branches, unused helpers
- [ ] **Context propagation**: request IDs/cancellation carried properly

### 5. Testing

Check for:
- [ ] **Coverage**: Critical paths tested
- [ ] **Edge cases**: nil, empty, boundary, timeout, cancellation
- [ ] **Concurrency tests** where shared state exists
- [ ] **External dependencies isolated** where needed
- [ ] **Assertions are specific** and failure messages actionable

## Review Output Format

```markdown
## Code Review: [file/component name]

### Summary
[1-2 sentence overview]

### Critical Issues
1. **[Issue]** (line X): [Description]
   - Impact: [What could go wrong]
   - Fix: [Suggested solution]

### Improvements
1. **[Suggestion]** (line X): [Description]

### Positive Notes
- [What was done well]

### Verdict
[ ] Ready to merge
[ ] Needs minor changes
[ ] Needs major revision
```

## Common Patterns to Flag

### Go
```go
// Bad: command injection
cmd := exec.Command("sh", "-c", "ls "+userInput)

// Good: avoid shell interpolation
cmd := exec.Command("ls", userInput)

// Bad: dropped error
data, _ := io.ReadAll(r)

// Good: handle and wrap
data, err := io.ReadAll(r)
if err != nil {
	return fmt.Errorf("read response body: %w", err)
}

// Bad: context not propagated
req, _ := http.NewRequest("GET", url, nil)

// Good: use context
req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
```

## Review Commands

```bash
# Code shape and staged changes
git diff --stat
git diff
git log --oneline -10

# Static checks
go test ./...
go test -race ./...
go vet ./...
staticcheck ./...
govulncheck ./...

# Fast text scans
rg -n "TODO|FIXME|HACK|XXX" .
rg -n "panic\\(|os\\.Exit\\(|log\\.Fatal" .
```

## Review Workflow

1. **Understand context**: Read PR description, linked issues
2. **Run checks**: `go test`, `go vet`, `staticcheck`, `govulncheck`
3. **Read top-down**: Start with main entry points
4. **Check tests**: Are behavior and edge cases covered?
5. **Security scan**: Validate unsafe input paths and secret handling
6. **Manual review**: Use checklist above
7. **Write feedback**: Be specific, suggest fixes, be kind
