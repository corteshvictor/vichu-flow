# Examples

VichuFlow is **stack-agnostic** — Go is only the language its binary is built
in. These minimal projects show how to point `vichu.yaml` at each ecosystem's
real verification commands. Copy one as a starting point.

| Stack | Folder | Gate command |
|---|---|---|
| Python | [`python/`](python) | `python3 -B -m unittest` (built-in; pytest also works) |
| Node / JS | [`node/`](node) | `node --test` (built-in runner; no package manager) |
| Go | [`go/`](go) | `go test ./...` |
| Rust | [`rust/`](rust) | `cargo test` |

Each folder has the stack's marker file (so `vichu init` auto-detects it), a
tiny piece of code with a test, and a `vichu.yaml` wired to that stack's gate.

To try one (you need that stack's toolchain installed for the gate to run):

```bash
cd examples/python
git init && git add -A && git commit -m "chore: seed"   # vichu needs a git repo
vichu run "add a multiply function"
```
