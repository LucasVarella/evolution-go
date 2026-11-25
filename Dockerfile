FROM golang:1.26.10-alpine AS build

RUN apk update && apk add --no-cache git build-base libjpeg-turbo-dev libwebp-dev

WORKDIR /build

# Copiar apenas arquivos de dependências primeiro para cachear o download
COPY go.mod go.sum ./

# whatsmeow agora vem do proxy oficial (go.mau.fi/whatsmeow, sem replace local) —
# não há mais submódulo whatsmeow-lib para copiar.
RUN go mod download

# Copiar o restante do código
COPY . .

ARG VERSION=dev
# Patch local no whatsmeow (figurinha com som: Message.audioStickerMessage, campo 134),
# equivalente ao PR upstream tulir/whatsmeow#1366. Remover quando o upstream tiver o suporte;
# se o patch deixar de aplicar após um update do whatsmeow, o build falha aqui de propósito.
RUN WM_DIR=$(go list -m -f '{{.Dir}}' go.mau.fi/whatsmeow) \
 && cp -r "$WM_DIR" /whatsmeow && chmod -R u+w /whatsmeow \
 && git -C /whatsmeow apply /build/patches/whatsmeow-audio-sticker.patch \
 && go mod edit -replace go.mau.fi/whatsmeow=/whatsmeow

RUN CGO_ENABLED=1 go build -ldflags "-X main.version=${VERSION}" -o server ./cmd/evolution-go

FROM alpine:3.19.1 AS final

# poppler-utils provides pdftoppm, used to rasterize PDF page 1 for /send/media document thumbnails
RUN apk update && apk add --no-cache tzdata ffmpeg libjpeg-turbo libwebp poppler-utils

WORKDIR /app

COPY --from=build /build/server .
COPY --from=build /build/manager/dist ./manager/dist
COPY --from=build /build/VERSION ./VERSION

ENV TZ=America/Sao_Paulo

ENTRYPOINT ["/app/server"]
