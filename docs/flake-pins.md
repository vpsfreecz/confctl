# Flake pins notes

In flake configs, confctl does not use any "core pins" concept.
Pin resolution comes from the flake lock file and the channel
mapping exported as `confctl.channels`.

Use normal flake inputs and the pins commands instead:
- confctl pins ls / update
- confctl pins channel ls / update
- confctl pins machine update

If you want a stable nixpkgs for helper evaluation, keep a normal
input like `nixpkgsCore` and point `nixpkgs` to it:

```
inputs = {
  nixpkgsCore.url = "...";
  nixpkgs.follows = "nixpkgsCore";
};
```

The helper uses `inputs.nixpkgs` for evaluation; machine roles still
select inputs via `confctl.channels`.
