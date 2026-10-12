# One image for both components:
#
#	docker run <image> receiver [-config path] [-listen addr]
#	docker run <image> forwarder [-config path]
#
# The binary is cross-compiled on the build platform, so building for another
# architecture (--platform) needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /relay ./cmd/relay

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /relay /relay
EXPOSE 8080
ENTRYPOINT ["/relay"]
