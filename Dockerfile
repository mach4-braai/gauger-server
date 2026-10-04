FROM golang:1.26.8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/gauger-server ./cmd/gauger-server \
 && mkdir -p /out/state

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gauger-server /gauger-server
COPY --from=build --chown=65532:65532 /out/state /var/lib/gauger-server
VOLUME /var/lib/gauger-server
ENTRYPOINT ["/gauger-server"]
