#!/bin/sh
# Directory standing in for cold object storage (S3/GCS). Init scripts run as the
# postgres OS user, so the server can write exports here with COPY ... TO PROGRAM.
set -e
mkdir -p /var/lib/postgresql/exports
