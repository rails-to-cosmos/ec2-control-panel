#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/deployments"
docker compose down --volumes --remove-orphans
docker compose up --build -d
