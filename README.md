# Clogs is a scoped, levelled, logging package, primarily intended for use with subprocesses

Package clogs provides a scoped, levelled, logging package where each scope is uniquely coloured. It supports subprocess
output redirection to a scope using a PTY, and handles most common CSI (ANSI) escape sequences, so that sub-processes
may output colour, move the cursor, and so on, and be handled correctly.

It does _not_ support concurrent access yet.

## What does it look like?

In the following image, each filename is a "scope". The margin will be dynamically resized based on the size of the terminal.

<img width="850" height="651" alt="image" src="https://github.com/user-attachments/assets/85008bef-c42d-49c7-bf1e-2265bf240343" />
