#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/deploy"
docker compose down --volumes --remove-orphans
docker compose up --build -d
