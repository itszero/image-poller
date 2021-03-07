FROM golang:1.16-alpine AS build

WORKDIR /src/
COPY go.* *.go /src/
RUN CGO_ENABLED=0 go build -o image-poller

FROM scratch
COPY --from=build /src/image-poller /app/image-poller
ENTRYPOINT ["/app/image-poller"]
