{
  description = "Local-first Google Messages CLI and archive";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = {
    self,
    nixpkgs,
  }: let
    supportedSystems = [
      "aarch64-darwin"
      "aarch64-linux"
      "x86_64-linux"
    ];
    forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
  in {
    packages = forAllSystems (
      system: let
        pkgs = nixpkgs.legacyPackages.${system};
        version = self.shortRev or self.dirtyShortRev or "dev";
        gmcli = pkgs.buildGoModule {
          pname = "gmcli";
          inherit version;
          src = ./.;

          vendorHash = "sha256-nUvK5VnWhBDjKNXGrFOfK6ZR43KegF+4tmFhY5brhfI=";

          ldflags = [
            "-s"
            "-w"
            "-X github.com/fdsouvenir/gmcli/cmd.Version=${version}"
          ];

          # Keep source-default assertions meaningful. buildGoModule otherwise
          # passes the release linker flags to both the binary and go test.
          preCheck = ''
            ldflags=(-buildid=)
          '';

          meta = {
            description = "Local-first Google Messages CLI and archive";
            homepage = "https://github.com/fdsouvenir/gmcli";
            license = pkgs.lib.licenses.agpl3Plus;
            mainProgram = "gmcli";
            platforms = supportedSystems;
          };
        };
        viewerSource = pkgs.lib.fileset.toSource {
          root = ./desktop;
          fileset = pkgs.lib.fileset.unions [
            ./desktop/Cargo.toml
            ./desktop/Cargo.lock
            ./desktop/Dioxus.toml
            ./desktop/src
            ./desktop/assets
          ];
        };
        gmcli-viewer = pkgs.rustPlatform.buildRustPackage {
          pname = "gmcli-viewer";
          inherit version;
          src = viewerSource;

          cargoLock.lockFile = ./desktop/Cargo.lock;

          nativeBuildInputs = with pkgs; [
            copyDesktopItems
            pkg-config
            wrapGAppsHook3
          ];
          buildInputs = with pkgs; [
            gtk3
            libappindicator-gtk3
            openssl
            webkitgtk_4_1
            xdotool
          ];

          desktopItems = [
            (pkgs.makeDesktopItem {
              name = "gmcli-viewer";
              desktopName = "gmcli Archive";
              comment = "Browse a local Google Messages JSONL archive";
              exec = "gmcli-viewer";
              categories = ["Utility"];
              terminal = false;
            })
          ];

          preFixup = ''
            gappsWrapperArgs+=(--set GMCLI_BIN ${pkgs.lib.getExe gmcli})
          '';

          meta = {
            description = "Local-first desktop viewer for gmcli JSONL archives";
            homepage = "https://github.com/fdsouvenir/gmcli";
            license = pkgs.lib.licenses.agpl3Plus;
            mainProgram = "gmcli-viewer";
            platforms = pkgs.lib.platforms.linux;
          };
        };
      in
        {
          inherit gmcli;
          default = gmcli;
        }
        // pkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
          inherit gmcli-viewer;
        }
    );

    apps = forAllSystems (
      system: let
        pkgs = nixpkgs.legacyPackages.${system};
      in
        {
          gmcli = {
            type = "app";
            program = nixpkgs.lib.getExe self.packages.${system}.gmcli;
            meta.description = "Run gmcli";
          };
          default = self.apps.${system}.gmcli;
        }
        // pkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
          viewer = {
            type = "app";
            program = nixpkgs.lib.getExe self.packages.${system}.gmcli-viewer;
            meta.description = "Browse the local gmcli archive";
          };
        }
    );

    overlays.default = final: _prev: {
      gmcli = self.packages.${final.stdenv.hostPlatform.system}.gmcli;
    };
  };
}
