# Portable container for the Dhakshina Desk API and worker (one binary).
# Build: docker build -t dhakshina-desk-api .
# Run:   docker run --env-file .env -p 8080:8080 dhakshina-desk-api
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dhakshina ./cmd/dhakshina

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dhakshina /dhakshina
ENV ROLE=all PORT=8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/dhakshina"]
CMD ["serve"]
