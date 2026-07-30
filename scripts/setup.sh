#!/usr/bin/env bash
#
# setup.sh — build maktoob and everything it needs to transcribe.
#
# What this does, in order:
#   1. checks for tools it cannot install for you
#   2. clones and builds whisper.cpp into vendor-build/
#   3. downloads one Whisper model (about 1.6 GB for the default)
#   4. builds the maktoob binary
#
# What it deliberately does not do:
#   - run sudo. It tells you the package command for your system and stops.
#     A setup script that escalates on its own is a setup script you cannot
#     read before trusting.
#   - start anything, or pair a WhatsApp account. Pairing links a real account
#     to this machine and is never a side effect of installing.
#
# Everything it downloads goes to vendor-build/ and models/, both gitignored.
# Delete those two directories to undo this script completely.

set -euo pipefail

MODEL="${MODEL:-large-v3-turbo}"
WHISPER_REF="${WHISPER_REF:-v1.7.4}"
JOBS="${JOBS:-$( (command -v nproc >/dev/null && nproc) || echo 4)}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
vendor="$repo_root/vendor-build"
whisper_src="$vendor/whisper.cpp"
models_dir="$repo_root/models"

bold=$'\033[1m'; dim=$'\033[2m'; red=$'\033[31m'; green=$'\033[32m'; off=$'\033[0m'
if [ ! -t 1 ]; then bold=""; dim=""; red=""; green=""; off=""; fi

step() { printf '\n%s==>%s %s%s\n' "$green" "$off" "$bold" "$*$off"; }
info() { printf '    %s%s%s\n' "$dim" "$*" "$off"; }
die()  { printf '\n%serror:%s %s\n' "$red" "$off" "$*" >&2; exit 1; }

# --- 1. dependencies -------------------------------------------------------

step "Checking dependencies"

install_hint() {
  if command -v apt-get >/dev/null 2>&1; then
    echo "sudo apt-get install -y $*"
  elif command -v dnf >/dev/null 2>&1; then
    echo "sudo dnf install -y $*"
  elif command -v pacman >/dev/null 2>&1; then
    echo "sudo pacman -S --needed $*"
  elif command -v brew >/dev/null 2>&1; then
    echo "brew install $*"
  else
    echo "install: $*"
  fi
}

missing=()
for tool in git cmake make ffmpeg; do
  if command -v "$tool" >/dev/null 2>&1; then
    info "found $tool"
  else
    missing+=("$tool")
  fi
done

if ! command -v go >/dev/null 2>&1; then
  die "Go is not installed. maktoob needs Go 1.25 or newer: https://go.dev/dl/"
fi
info "found go ($(go version | awk '{print $3}'))"

if [ ${#missing[@]} -gt 0 ]; then
  printf '\n%smissing:%s %s\n\n' "$red" "$off" "${missing[*]}"
  printf 'Install them with:\n\n    %s\n\n' "$(install_hint "${missing[@]}")"
  die "cannot continue without the tools above"
fi

# ffmpeg is a hard requirement at runtime, not only at build time: every note
# goes through it on the way to 16 kHz mono wav. Checking here means the failure
# surfaces now rather than on the user's first real voice note.
info "ffmpeg: $(ffmpeg -version 2>/dev/null | head -1 | cut -c1-60)"

# --- 2. whisper.cpp --------------------------------------------------------

step "Building whisper.cpp ($WHISPER_REF)"

mkdir -p "$vendor"

server_bin="$whisper_src/build/bin/whisper-server"

# An existing checkout is left exactly as it is — no fetch, no checkout, no
# rebuild. This tree is gigabytes and takes minutes to compile, and someone
# re-running setup.sh to fix an unrelated step should not have a working ASR
# backend moved to a different commit underneath them. Delete vendor-build/ to
# force a clean install.
if [ -x "$server_bin" ]; then
  info "whisper-server already built at $server_bin"
  info "leaving the existing checkout alone; delete vendor-build/ to rebuild"
else
  if [ -d "$whisper_src/.git" ]; then
    info "checkout exists but has no built server; building it as it stands"
  else
    # Pinned to a tag rather than tracking master. An ASR backend that changes
    # under the project between two runs makes any accuracy number meaningless.
    git clone --depth 1 --branch "$WHISPER_REF" \
      https://github.com/ggml-org/whisper.cpp "$whisper_src"
  fi

  info "compiling with $JOBS job(s); this takes a few minutes"
  cmake -S "$whisper_src" -B "$whisper_src/build" -DCMAKE_BUILD_TYPE=Release >/dev/null
  cmake --build "$whisper_src/build" --config Release -j "$JOBS" --target whisper-server

  [ -x "$server_bin" ] || die "whisper-server was not produced at $server_bin"
  info "built $server_bin"
fi

# --- 3. model --------------------------------------------------------------

step "Fetching the $MODEL model"

model_file="$models_dir/ggml-$MODEL.bin"
vendored_model="$whisper_src/models/ggml-$MODEL.bin"

if [ -s "$model_file" ]; then
  info "already present ($(du -h "$model_file" | cut -f1))"
elif [ -s "$vendored_model" ]; then
  # whisper.cpp's downloader leaves the model inside its own tree. Using it
  # where it lies costs nothing and saves re-fetching well over a gigabyte that
  # is already on the disk.
  model_file="$vendored_model"
  info "using the copy already in the whisper.cpp tree ($(du -h "$model_file" | cut -f1))"
else
  mkdir -p "$models_dir"
  info "downloading; the default model is about 1.6 GB"
  # whisper.cpp's own downloader, so the URL and checksum stay its problem
  # rather than something this script has to keep correct.
  ( cd "$whisper_src" && bash ./models/download-ggml-model.sh "$MODEL" )
  mv "$vendored_model" "$model_file"
  info "saved to $model_file"
fi

# --- 4. maktoob ------------------------------------------------------------

step "Building maktoob"

( cd "$repo_root" && go build -o maktoob ./cmd/maktoob )
info "built $repo_root/maktoob"

# --- done ------------------------------------------------------------------

cat <<EOF

${green}Done.${off}

Start the transcription backend in one terminal:

    $server_bin \\
        --model $model_file \\
        --host 127.0.0.1 --port 8642

And maktoob in another:

    $repo_root/maktoob serve

Then open http://127.0.0.1:8765

To transcribe a file without WhatsApp:

    $repo_root/maktoob import path/to/audio.ogg

To link a WhatsApp account, run ${bold}maktoob pair${off} and scan the QR code.
Read the Privacy section of the README first — pairing grants a full
multi-device session, not only voice notes.
EOF
