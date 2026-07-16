# Embedded UI staging area

Run `./scripts/stage-go-ui.sh` from the repository root to build the existing React UI and
copy its output to `internal/ui/dist/`. The generated directory is ignored by Git. A later
migration phase will add the Go package and `go:embed` directive that consume these files.
