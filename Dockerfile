# Build: docker build -t phoenix .
# Run the UI (Linux, host networking because the UI only binds loopback):
#   docker run --rm --network host -v phoenix-data:/data phoenix
# CLI: docker run --rm --network host -v "$PWD:/data" phoenix run scenarios/x.json
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/phoenix ./cmd/phoenix

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/phoenix /usr/local/bin/phoenix
ENV PHOENIX_HOME=/data/.phoenix
WORKDIR /data
USER nonroot
ENTRYPOINT ["/usr/local/bin/phoenix"]
CMD ["serve", "--addr", "127.0.0.1:7878", "--workspace", "/data/phoenix-workspace"]
