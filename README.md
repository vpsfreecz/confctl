# confctl
confctl is a Nix deployment configuration management tool. It can be used to
build and deploy [NixOS](https://nixos.org) and [vpsAdminOS](https://vpsadminos.org)
machines.

## Features

* Stateless
* Per-machine nixpkgs (both modules and packages)
* Support for configuration interconnections (declare and access other machines'
  configurations)

## Requirements

* [Nix](https://nixos.org)

## Usage
1. Create a new directory, where the new configuration will be stored:

```
mkdir cluster-configuration
```

2. Create a `shell.nix`:
```
cd cluster-configuration
cat > shell.nix <<EOF
import "${builtins.fetchTarball https://github.com/vpsfreecz/confctl/archive/master.tar.gz}/shell.nix"
EOF
```

3. Enter the `nix-shell`. This will install confctl and its dependencies
into `.gems/`:
```
nix-shell
```

From within the shell, you can access the manual:

```
man confctl
```

4. Initialize the configuration directory with confctl:
```
confctl init
```

5. Add a new machine to be deployed:
```
confctl add my-machine
```

You can now edit the machine's configuration in directory `cluster/my-machine`.

6. Configure software pins:
```
# Create a channel with software pins that can be reused by multiple machines
confctl swpins channel new nixos-unstable

# Add unstable nixpkgs to the new channel, this will take a long time
confctl swpins channel git add nixos-unstable nixpkgs https://github.com/NixOS/nixpkgs refs/heads/nixos-unstable

# Let the machine user our channel
confctl swpins file channel use my-machine nixos-unstable
```

7. Build the machine
```
confctl build my-machine
```

8. Deploy the machine
```
confctl deploy my-machine
```
