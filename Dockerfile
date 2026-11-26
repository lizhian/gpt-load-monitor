FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
COPY web ./web
RUN go test ./...
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/dashboard .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dashboard /dashboard
ENV ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/dashboard"]
