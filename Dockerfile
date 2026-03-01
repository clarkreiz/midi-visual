FROM golang:1.24-bookworm AS builder

WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends libasound2-dev && rm -rf /var/lib/apt/lists/*

COPY backend/go.mod backend/go.sum ./
RUN go mod download

COPY backend/ .
RUN CGO_ENABLED=1 GOOS=linux go build -o miviz .


FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends libasound2 && rm -rf /var/lib/apt/lists/*

COPY --from=builder /src/miviz /miviz

EXPOSE 8080
ENTRYPOINT ["/miviz"]
