{
  description = "memebox: a self-hosted meme, GIF and sticker library that converts for every chat app";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
        "x86_64-darwin"
      ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAll (pkgs: rec {
        memebox = pkgs.callPackage ./nix/package.nix { };
        default = memebox;
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            ffmpeg
            imagemagick
          ];
          MEMEBOX_FONT = "${pkgs.dejavu_fonts}/share/fonts/truetype/DejaVuSans-Bold.ttf";
        };
      });

      nixosModules.default = import ./nix/module.nix self;
    };
}
