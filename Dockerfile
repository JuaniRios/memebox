FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /memebox .

FROM alpine:3.22
RUN apk add --no-cache ffmpeg imagemagick imagemagick-heic imagemagick-webp imagemagick-jpeg font-dejavu ca-certificates
COPY --from=build /memebox /usr/local/bin/memebox
ENV MEMEBOX_ADDR=0.0.0.0:8790 \
    MEMEBOX_DATA=/data \
    MEMEBOX_FONT=/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf
RUN mkdir /data && chown 1000:1000 /data
VOLUME /data
EXPOSE 8790
USER 1000:1000
ENTRYPOINT ["memebox"]
