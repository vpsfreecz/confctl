{
  pkgs,
  oracle,
  native,
  tools,
  oracleSrc,
  nixpkgsSrc,
}:
let
  oracleInput = builtins.fetchTree {
    type = "path";
    path = "${oracleSrc}";
  };
  configs = pkgs.lib.genAttrs [ "1" "10" "100" "1000" ] (
    n:
    import ./fixture.nix {
      inherit pkgs oracleSrc nixpkgsSrc;
      count = pkgs.lib.toInt n;
    }
  );
  configurationTrees = pkgs.lib.genAttrs [ "1" "10" "100" "1000" ] (
    n:
    builtins.fetchTree {
      type = "path";
      path = "${configs.${n}}";
    }
  );
  configurationSources = pkgs.lib.genAttrs [ "1" "10" "100" "1000" ] (n: {
    sourcePath = configurationTrees.${n}.outPath;
    narHash = configurationTrees.${n}.narHash;
  });
  evaluation =
    n:
    import (oracleSrc + "/nix/flake/mk-confctl-outputs.nix") {
      confDir = configurationTrees.${n}.outPath;
      inputs = {
        self.outPath = configurationTrees.${n}.outPath;
        confctl = oracleInput;
        nixpkgs = ((import (nixpkgsSrc + "/flake.nix")).outputs { self.outPath = nixpkgsSrc; }) // {
          outPath = nixpkgsSrc;
        };
      };
      system = pkgs.stdenv.hostPlatform.system;
      channels.fixture.nixpkgs = "nixpkgs";
    };
  metadata = map (n: (evaluation n).machinesJson) [
    "1"
    "10"
    "100"
    "1000"
  ];
  key = (evaluation "1").machineKeys."lab/nodes/node0001";
  seed = (evaluation "1").build.${key};
  expectedRuntime = {
    schema = 1;
    scales = pkgs.lib.genAttrs [ "1" "10" "100" "1000" ] (
      n:
      let
        c = evaluation n;
        identity = d: {
          outputPath = d.outPath;
          # A digest identifies the derivation without adding its closure.
          drvPathSha256 = builtins.hashString "sha256" d.drvPath;
        };
      in
      {
        settings = c.settings;
        machineKeys = c.machineKeys;
        metadata = identity c.machinesJson;
        metadataSha256 = builtins.hashString "sha256" (builtins.toJSON c.machines);
        seed =
          if n == "1" then
            {
              inherit key;
              toplevel = identity c.build.${key}.toplevel;
              autoRollback = identity c.build.${key}.autoRollback;
            }
          else
            null;
      }
    );
  };
  packageManifest = {
    contractRevision = "cc40679d267165aecfa569128438bb55fe910268";
    inherit
      oracleSrc
      nixpkgsSrc
      oracle
      native
      tools
      configs
      configurationSources
      metadata
      key
      expectedRuntime
      ;
    seed = { inherit (seed) toplevel autoRollback; };
  };
  binaries = pkgs.buildEnv {
    name = "confctl-real-local-binaries";
    paths = [
      pkgs.nix
      pkgs.openssh
      pkgs.git
      pkgs.bashInteractive
      pkgs.coreutils
      pkgs.gawk
      pkgs.gnugrep
      pkgs.iproute2
      pkgs.strace
    ];
    pathsToLink = [ "/bin" ];
  };
  rootfs = pkgs.runCommand "confctl-real-local-rootfs" { nativeBuildInputs = [ pkgs.openssl ]; } ''
        mkdir -p "$out"/{bin,etc/nix,work,root,run/fixture,tmp,var/empty,nix/store,nix/var/nix/gcroots/per-user/root}
        for bin in ${binaries}/bin/*; do ln -s "$bin" "$out/bin/$(basename "$bin")"; done
        ln -sfn ${pkgs.bashInteractive}/bin/bash "$out/bin/sh"
        ln -s ${tools}/bin/compat-real "$out/bin/compat-real"
        cp ${./entry.sh} "$out/bin/fixture-run"
        chmod +x "$out/bin/fixture-run"
        ${pkgs.lib.concatStringsSep "\n" (
          map (n: ''cp -a ${configs.${n}} "$out/work/config-${n}"; chmod -R u+w "$out/work/config-${n}"'') [
            "1"
            "10"
            "100"
            "1000"
          ]
        )}
        ${pkgs.lib.concatStringsSep "\n" (
          map
            (n: ''
              test "$(sha256sum ${(evaluation n).machinesJson} | cut -d ' ' -f 1)" = '${
                expectedRuntime.scales.${n}.metadataSha256
              }'
            '')
            [
              "1"
              "10"
              "100"
              "1000"
            ]
        )}
        echo 'root:x:0:0:Fixture root:/root:/bin/bash' > "$out/etc/passwd"
        echo 'sshd:x:74:74:SSH privilege separation:/var/empty:/bin/false' >> "$out/etc/passwd"
        printf 'root:x:0:\nsshd:x:74:\n' > "$out/etc/group"
        printf 'root:%s:1:0:99999:7:::\n' "$(openssl passwd -6 -salt confctl-fixture fixture-only-disabled)" > "$out/etc/shadow"
        chmod 600 "$out/etc/shadow"
        printf 'passwd: files\ngroup: files\nhosts: files\n' > "$out/etc/nsswitch.conf"
        printf '127.0.0.1 localhost\n::1 localhost\n' > "$out/etc/hosts"
        cat > "$out/etc/nix/nix.conf" <<'NIXCONF'
    experimental-features = nix-command flakes
    build-users-group =
    sandbox = false
    substituters =
    builders =
    max-jobs = 0
    NIXCONF
        cat > "$out/etc/confctl-fixture.env" <<'ENV'
    export CONFCTL_FIXTURE_RUBY='${oracle.ruby}/bin/ruby'
    export CONFCTL_REAL_ORACLE='${oracle}/bin/confctl'
    export CONFCTL_REAL_NATIVE='${native}/bin/confctl-go-prototype'
    export CONFCTL_FIXTURE_SEED='${seed.toplevel}'
    export CONFCTL_FIXTURE_ROLLBACK='${seed.autoRollback}'
    export CONFCTL_FIXTURE_KEY='${key}'
    ENV
        cp ${pkgs.writeText "package-manifest.json" (builtins.toJSON packageManifest)} "$out/etc/confctl-fixture-packages.json"
        chmod 1777 "$out/tmp"
  '';
in
{
  inherit
    configs
    configurationTrees
    configurationSources
    metadata
    seed
    key
    expectedRuntime
    packageManifest
    ;
  image = pkgs.dockerTools.buildLayeredImage {
    name = "confctl-real-local";
    tag = "measurement-1";
    includeNixDB = true;
    contents = [ rootfs ];
    # symlinkJoin turns the working files into store links; Git/Nix need copies.
    extraCommands = pkgs.lib.concatStringsSep "\n" (
      map
        (n: ''
          test -d "work/config-${n}"
          test ! -L "work/config-${n}"
          cp -a --remove-destination ${configs.${n}}/. "work/config-${n}/"
          test -z "$(find "work/config-${n}" ! -type d ! -type f -print -quit)"
          diff -r --no-dereference ${configs.${n}} "work/config-${n}"
          chmod -R u+w "work/config-${n}"
          test -z "$(find "work/config-${n}" ! -perm -u+w -print -quit)"
        '')
        [
          "1"
          "10"
          "100"
          "1000"
        ]
    );
    config = {
      Cmd = [
        "/bin/fixture-run"
        "/artifacts/run.json"
      ];
      WorkingDir = "/work";
      Env = [
        "PATH=/bin"
        "USER=root"
        "HOME=/root"
        "NIX_REMOTE=local"
        "CONFCTL_TTY=0"
        "NO_COLOR=1"
        "PAGER="
        "TZ=UTC"
        "LANG=C.UTF-8"
      ];
    };
  };
}
