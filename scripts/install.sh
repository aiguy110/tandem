#!/bin/sh
# Install the latest Tandem standalone Linux binary into ~/.local/bin.
set -eu

repository="aiguy110/tandem"
install_dir="${HOME}/.local/bin"

case "$(uname -s)" in
  Linux) os="linux" ;;
  *)
    echo "Tandem standalone releases currently support Linux only." >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *)
    echo "Unsupported CPU architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

asset="tandem_${os}_${arch}"
base_url="https://github.com/${repository}/releases/latest/download/${asset}"
temporary_dir="$(mktemp -d)"
cleanup() {
  rm -rf "$temporary_dir"
}
trap cleanup EXIT HUP INT TERM

echo "Downloading the latest Tandem release for ${os}/${arch}..."
curl --fail --location --silent --show-error "$base_url" -o "$temporary_dir/$asset"
curl --fail --location --silent --show-error "$base_url.sha256" -o "$temporary_dir/$asset.sha256"

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$temporary_dir" && sha256sum -c "$asset.sha256")
elif command -v shasum >/dev/null 2>&1; then
  expected="$(awk '{print $1}' "$temporary_dir/$asset.sha256")"
  actual="$(shasum -a 256 "$temporary_dir/$asset" | awk '{print $1}')"
  [ "$actual" = "$expected" ] || {
    echo "Checksum verification failed." >&2
    exit 1
  }
else
  echo "Need sha256sum or shasum to verify the release download." >&2
  exit 1
fi

mkdir -p "$install_dir"
install -m 0755 "$temporary_dir/$asset" "$install_dir/tandem"
echo "Installed Tandem to $install_dir/tandem"

case ":${PATH}:" in
  *":${install_dir}:"*) ;;
  *)
    case "${SHELL:-}" in
      */zsh) shell_rc="${HOME}/.zshrc" ;;
      *) shell_rc="${HOME}/.bashrc" ;;
    esac
    path_line='export PATH="$HOME/.local/bin:$PATH"'
    if [ ! -f "$shell_rc" ] || ! grep -Fqx "$path_line" "$shell_rc"; then
      printf '\n# Added by Tandem installer\n%s\n' "$path_line" >> "$shell_rc"
      echo "Added ~/.local/bin to PATH in $shell_rc. Run: source $shell_rc"
    fi
    ;;
esac
