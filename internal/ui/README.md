# Embedded UI staging area

Run `./scripts/stage-go-ui.sh` from the repository root to build the React UI and copy its
output to `internal/ui/dist/`. The generated directory is ignored by Git and consumed by
`embed_prod.go`; the production Go executable contains the complete frontend.

Use `go build -tags tandem_dev` for a lightweight executable without embedded UI. That
build serves `TANDEM_UI_DIR` when configured and otherwise displays the daemon placeholder.
