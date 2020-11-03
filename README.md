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
1. There are no releases or packages yet, so first clone the repository:
```
git clone https://github.com/vpsfreecz/confctl
```

2. Create a new directory, where your confctl-managed configuration will be
stored:

```
mkdir cluster-configuration
```

3. Create a `shell.nix` in the new directory and import the same file
from confctl:
```
cd cluster-configuration
cat > shell.nix <<EOF
import "/the-location-of-your-confctl-repository/shell.nix"
EOF
```

4. Enter the `nix-shell`. This will make confctl available and install its
dependencies into `.gems/`:
```
nix-shell
```

From within the shell, you can access the manual:

```
man confctl
```

5. Initialize the configuration directory with confctl:
```
confctl init
```

6. Add a new machine to be deployed:
```
confctl add my-machine
```

You can now edit the machine's configuration in directory `cluster/my-machine`.

7. Configure software pins:
```
# Create a channel with software pins that can be reused by multiple machines
confctl swpins channel new nixos-unstable

# Add unstable nixpkgs to the new channel, this will take a long time
confctl swpins channel git add nixos-unstable nixpkgs https://github.com/NixOS/nixpkgs refs/heads/nixos-unstable

# Let the machine user our channel
confctl swpins file channel use my-machine nixos-unstable
```

8. Build the machine
```
confctl build my-machine
```

9. Deploy the machine
```
confctl deploy my-machine
```
