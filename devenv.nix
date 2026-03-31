{ pkgs, ... }:

{
  languages.go.enable = true;

  packages = [
    pkgs.air
  ];

  env = {
    PORT = "8080";
  };

  processes.dev.exec = "air";
}
