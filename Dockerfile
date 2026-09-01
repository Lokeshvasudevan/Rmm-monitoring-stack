FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# Generate/verify dependency checksums before compiling. This also supports a
# fresh checkout while go.sum is being introduced to the repository.
RUN go mod tidy && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/redfish-exporter ./cmd/exporter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/redfish-exporter /redfish-exporter
EXPOSE 9610
ENTRYPOINT ["/redfish-exporter"]
