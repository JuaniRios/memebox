{
  lib,
  buildGoModule,
  makeWrapper,
  ffmpeg,
  imagemagick,
  dejavu_fonts,
}:
buildGoModule {
  pname = "memebox";
  version = "0.1.0";
  src = lib.cleanSource ./..;
  # Standard library only.
  vendorHash = null;
  nativeBuildInputs = [ makeWrapper ];
  postInstall = ''
    wrapProgram $out/bin/memebox \
      --prefix PATH : ${
        lib.makeBinPath [
          ffmpeg
          imagemagick
        ]
      } \
      --set-default MEMEBOX_FONT ${dejavu_fonts}/share/fonts/truetype/DejaVuSans-Bold.ttf
  '';
  meta = {
    description = "Self-hosted meme, GIF and sticker library that converts for every chat app";
    mainProgram = "memebox";
    license = lib.licenses.mit;
  };
}
