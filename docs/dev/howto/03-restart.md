# How to restart Ubuntu Pro for WSL

Some configuration changes only apply when you restart Ubuntu Pro for WSL. Here is a guide on how to restart it.

## Option 1: Restart your machine

This is the simple one. If you're not in a hurry to see the configuration updated, just wait until next time you boot your machine.

## Option 2: Restart only Ubuntu Pro for WSL

1. Stop the agent:

    ```powershell
    Get-Process -Name Ubuntu-Pro-Agent | Stop-Process
    ```

2. Stop the distro, or distros you installed WSL-Pro-Service in:

    ```powershell
    wsl --terminate DISTRO_NAME_1
    wsl --terminate DISTRO_NAME_2
    # etc.

    # Alternatively, stop all distros:
    wsl --shutdown
    ```

7. Start the agent again:
    1. Open the start Menu and search for "Ubuntu Pro for WSL".
    2. The GUI should start.
    3. Wait a minute.
    4. Click on "Click to restart it".
8. Start the distro, or distros you installed WSL-Pro-Service in.
