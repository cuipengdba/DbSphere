FROM --platform=$BUILDPLATFORM golang:1.27.4-bookworm AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY examples/trace-lock-app ./examples/trace-lock-app
COPY internal/trace ./internal/trace
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/trace-lock-app ./examples/trace-lock-app

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/trace-lock-app /trace-lock-app
USER 65532:65532
EXPOSE 18092
ENTRYPOINT ["/trace-lock-app"]
