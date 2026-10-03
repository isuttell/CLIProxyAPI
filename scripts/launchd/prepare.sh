#!/bin/sh
set -eu

usage() {
    echo "Usage: $0 LABEL BINARY CONFIG WORKING_DIRECTORY LOG_DIRECTORY PLIST" >&2
    exit 1
}

fail() {
    echo "$*" >&2
    exit 1
}

[ "$(uname -s)" = Darwin ] || fail "launchd support requires macOS."
[ "$#" -eq 6 ] || usage
label=$1
binary=$2
config=$3
working_directory=$4
log_directory=$5
plist=$6

case "$label" in
    ''|*[!A-Za-z0-9.-]*) fail "Label must contain only letters, digits, dots, and hyphens." ;;
esac
for path in "$binary" "$config" "$working_directory" "$log_directory" "$plist"; do
    case "$path" in
        /*) ;;
        *) fail "Every path must be absolute." ;;
    esac
done
[ -f "$binary" ] && [ -x "$binary" ] || fail "Binary must be an executable file."
[ -f "$config" ] && [ -r "$config" ] || fail "Configuration must be a readable file."
[ -d "$working_directory" ] || fail "Working directory does not exist."
[ -d "$(dirname "$plist")" ] || fail "Create the plist parent directory first."
[ ! -e "$plist" ] && [ ! -L "$plist" ] || fail "Refusing to overwrite an existing plist."

umask 077
temporary=$(mktemp "$(dirname "$plist")/.launchd.XXXXXX")
trap 'rm -f "$temporary"' EXIT HUP INT TERM

# plutil escapes paths without shell or XML interpolation.
/usr/bin/plutil -create xml1 "$temporary"
/usr/bin/plutil -insert Label -string "$label" "$temporary"
/usr/bin/plutil -insert ProgramArguments -array "$temporary"
/usr/bin/plutil -insert ProgramArguments.0 -string "$binary" "$temporary"
/usr/bin/plutil -insert ProgramArguments.1 -json '"--config"' "$temporary"
/usr/bin/plutil -insert ProgramArguments.2 -string "$config" "$temporary"
/usr/bin/plutil -insert WorkingDirectory -string "$working_directory" "$temporary"
/usr/bin/plutil -insert RunAtLoad -bool YES "$temporary"
/usr/bin/plutil -insert KeepAlive -bool YES "$temporary"
/usr/bin/plutil -insert ThrottleInterval -integer 10 "$temporary"
/usr/bin/plutil -insert StandardOutPath -string "$log_directory/stdout.log" "$temporary"
/usr/bin/plutil -insert StandardErrorPath -string "$log_directory/stderr.log" "$temporary"
/usr/bin/plutil -insert EnvironmentVariables -dictionary "$temporary"
/usr/bin/plutil -insert EnvironmentVariables.GOTRACEBACK -string all "$temporary"
/usr/bin/plutil -lint "$temporary"
mkdir -p "$log_directory"
# A hard link publishes atomically and fails if another writer created the destination.
ln "$temporary" "$plist"
echo "Prepared $plist. No service was loaded or stopped."
