self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.memebox;
in
{
  options.services.memebox = {
    enable = lib.mkEnableOption "memebox, a self-hosted meme library";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.memebox;
      defaultText = lib.literalExpression "memebox.packages.\${system}.memebox";
    };

    address = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1";
      description = "Address to listen on. Keep it on loopback and put Tailscale Serve or a reverse proxy in front.";
    };

    port = lib.mkOption {
      type = lib.types.port;
      default = 8790;
    };

    passwordFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "File holding the login password. Null leaves the app open to anyone who can reach it, which suits a tailnet.";
    };

    maxUploadMB = lib.mkOption {
      type = lib.types.ints.positive;
      default = 200;
    };

    tailscaleServe = {
      enable = lib.mkEnableOption "publishing memebox on the tailnet with Tailscale Serve (HTTPS, tailnet only)";
      httpsPort = lib.mkOption {
        type = lib.types.port;
        default = 443;
        description = "Tailnet HTTPS port. Tailscale Serve accepts 443, 8443 and 10000.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.memebox = {
      description = "memebox meme library";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      environment = {
        MEMEBOX_ADDR = "${cfg.address}:${toString cfg.port}";
        MEMEBOX_DATA = "/var/lib/memebox";
        MEMEBOX_MAX_UPLOAD_MB = toString cfg.maxUploadMB;
      }
      # %d is systemd's credentials directory, where LoadCredential puts it.
      // lib.optionalAttrs (cfg.passwordFile != null) { MEMEBOX_PASSWORD_FILE = "%d/password"; };
      serviceConfig = {
        ExecStart = lib.getExe cfg.package;
        DynamicUser = true;
        StateDirectory = "memebox";
        LoadCredential = lib.optional (cfg.passwordFile != null) "password:${cfg.passwordFile}";
        Restart = "on-failure";
        # ffmpeg is the heavy part; keep it from crowding out other work.
        Nice = 5;
        IOSchedulingClass = "best-effort";
        IOSchedulingPriority = 6;
        NoNewPrivileges = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        LockPersonality = true;
        SystemCallArchitectures = "native";
      };
    };

    systemd.services.memebox-tailscale-serve = lib.mkIf cfg.tailscaleServe.enable {
      description = "Publish memebox with Tailscale Serve";
      wantedBy = [ "multi-user.target" ];
      after = [
        "tailscaled.service"
        "memebox.service"
      ];
      wants = [ "tailscaled.service" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        Restart = "on-failure";
        RestartSec = 15;
      };
      # Tailscale stores the serve config itself; this re-applies it so the
      # declared port wins after a rebuild or a tailscale state reset.
      script = ''
        ${lib.getExe config.services.tailscale.package} serve --bg \
          --https=${toString cfg.tailscaleServe.httpsPort} \
          http://${cfg.address}:${toString cfg.port}
      '';
      preStop = ''
        ${lib.getExe config.services.tailscale.package} serve \
          --https=${toString cfg.tailscaleServe.httpsPort} off || true
      '';
    };
  };
}
