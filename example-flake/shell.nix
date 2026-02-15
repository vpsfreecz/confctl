# When a part of confctl repository
import ../shell.nix

# When copied elsewhere
# if builtins.pathExists ../confctl/shell.nix then
#   import ../confctl/shell.nix
# else if builtins.pathExists ../../confctl/shell.nix then
#   import ../../confctl/shell.nix
# else if builtins.pathExists /where-is-your-confctl/shell.nix then
#   import /where-is-your-confctl/shell.nix
# else builtins.abort "Unable to find confctl shell"
