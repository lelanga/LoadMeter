# Build: docker build -t loadmeter .
# Run the UI (Linux, host networking because the UI only binds loopback):
#   docker run --rm --network host -v loadmeter-data:/data loadmeter
# CLI: docker run --rm --network host -v "$PWD:/data" loadmeter run scenarios/x.json
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/loadmeter ./cmd/loadmeter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/loadmeter /usr/local/bin/loadmeter
ENV LOADMETER_HOME=/data/.loadmeter
WORKDIR /data
USER nonroot
ENTRYPOINT ["/usr/local/bin/loadmeter"]
CMD ["serve", "--addr", "127.0.0.1:7878", "--workspace", "/data/loadmeter-workspace"]
