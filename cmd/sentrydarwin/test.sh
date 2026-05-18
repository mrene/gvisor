#!/bin/bash
# gVisor macOS port test suite — bash wrapper for test.py
# Usage: ./cmd/sentrydarwin/test.sh [rootfs_path]
exec python3 "$(dirname "$0")/test.py" "$@"
