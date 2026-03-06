/*
 * vsock_listen.c — Minimal vsock listener for Firecracker guest VMs.
 *
 * Listens on vsock port 52. When the host connects (after migration),
 * runs /usr/local/bin/boxcutter-nudge to re-establish network paths.
 */
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <sys/socket.h>
#include <linux/vm_sockets.h>

#define VSOCK_PORT 52

int main(void) {
	int s = socket(AF_VSOCK, SOCK_STREAM, 0);
	if (s < 0) { perror("vsock socket"); return 1; }

	struct sockaddr_vm addr = {
		.svm_family = AF_VSOCK,
		.svm_cid = VMADDR_CID_ANY,
		.svm_port = VSOCK_PORT,
	};

	if (bind(s, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
		perror("vsock bind");
		return 1;
	}
	if (listen(s, 1) < 0) {
		perror("vsock listen");
		return 1;
	}

	for (;;) {
		int c = accept(s, NULL, NULL);
		if (c < 0) continue;
		write(c, "OK\n", 3);
		close(c);
		system("/usr/local/bin/boxcutter-nudge");
	}
}
