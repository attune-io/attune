# hack/demo.tape - VHS (Charm) recording script for kube-rightsize demo.
#
# Install VHS: go install github.com/charmbracelet/vhs@latest
# Record:      vhs hack/demo.tape
#
# This produces a GIF/MP4 of the demo running in a real terminal.
# The demo script must complete within the timeouts below.
# Use --no-cleanup to keep the cluster after recording.

Output hack/demo.gif
Output hack/demo.mp4

Set FontSize 14
Set Width 1200
Set Height 800
Set Theme "Catppuccin Mocha"
Set TypingSpeed 50ms
Set Padding 20

Type "# kube-rightsize: safe in-place Kubernetes pod right-sizing"
Enter
Sleep 1s

Type "# Let's see it in action on a real cluster."
Enter
Sleep 1s

Type "./hack/demo.sh --no-cleanup"
Enter
Sleep 600s