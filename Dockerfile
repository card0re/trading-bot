# Образ сборщика стакана Binance Prediction (cmd/predictrec) для Cloud Run.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /predictrec ./cmd/predictrec

FROM gcr.io/distroless/static-debian12
COPY --from=build /predictrec /predictrec
ENTRYPOINT ["/predictrec", "-out", "/data"]
