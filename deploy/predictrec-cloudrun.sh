#!/usr/bin/env bash
# Деплой сборщика стакана Binance Prediction (cmd/predictrec) на Cloud Run.
# Один постоянно живой инстанс (CPU всегда выделен), CSV по часам пишутся в
# бакет GCS, примонтированный в /data. ~$1.7/сутки (1 vCPU, 512 МБ, europe-west1).
#
#   PROJECT=my-project BUCKET=my-predict-data ./deploy/predictrec-cloudrun.sh
#
# Забрать данные:  gsutil -m cp "gs://$BUCKET/*.csv" ./data/predict/
# Остановить:      gcloud run services delete predictrec --region "$REGION" --project "$PROJECT"
set -euo pipefail
: "${PROJECT:?укажи PROJECT}"
: "${BUCKET:?укажи BUCKET}"
REGION="${REGION:-europe-west1}" # не США: binance.com режет американские IP

gcloud services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com storage.googleapis.com --project "$PROJECT"
gcloud storage buckets describe "gs://$BUCKET" --project "$PROJECT" >/dev/null 2>&1 ||
  gcloud storage buckets create "gs://$BUCKET" --project "$PROJECT" --location "$REGION" --uniform-bucket-level-access

gcloud run deploy predictrec --quiet \
  --project "$PROJECT" --region "$REGION" --source . \
  --no-allow-unauthenticated \
  --min-instances 1 --max-instances 1 --no-cpu-throttling \
  --cpu 1 --memory 512Mi \
  --add-volume name=data,type=cloud-storage,bucket="$BUCKET" \
  --add-volume-mount volume=data,mount-path=/data
