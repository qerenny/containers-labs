# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/batch .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/batch /batch
USER 65532:65532
ENTRYPOINT ["/batch"]
