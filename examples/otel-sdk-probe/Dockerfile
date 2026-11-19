# Build the static acceptance binary with the host Go toolchain first:
# CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o sdk-oom-app .
FROM scratch
COPY examples/otel-sdk-probe/sdk-oom-app /otel-sdk-probe
USER 65532:65532
EXPOSE 18093
ENTRYPOINT ["/otel-sdk-probe"]
