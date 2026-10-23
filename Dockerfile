# Build the Go binaries, then run them on a slim image with LaTeX for server-side math/chemfig rendering.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM debian:bookworm-slim
# LaTeX + chemfig/mhchem + dvisvgm render math and ```chemfig blocks in answers to SVG (internal/latexsvg);
# catdoc reads old .doc/.ppt/.xls files (internal/filetext)
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates texlive-latex-base texlive-pictures texlive-science \
    texlive-plain-generic dvisvgm catdoc \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
ENV CANVAS_GO_ROOT=/app
EXPOSE 8765
CMD ["ws_server"]
