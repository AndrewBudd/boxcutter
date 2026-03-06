/*
 * libnss_catchall — NSS module that maps any unknown username to dev (uid 1000).
 * This allows SSH login with any username, all sharing /home/dev.
 *
 * Build: gcc -shared -fPIC -o libnss_catchall.so.2 nss_catchall.c
 * Install: cp libnss_catchall.so.2 /usr/lib/x86_64-linux-gnu/
 * Configure: edit /etc/nsswitch.conf — passwd: files catchall
 *                                      shadow: files catchall
 */
#include <nss.h>
#include <pwd.h>
#include <shadow.h>
#include <string.h>
#include <errno.h>

static const char *system_users[] = {
    "root", "dev", "nobody", "sshd", "messagebus", "avahi",
    "syslog", "systemd-network", "systemd-resolve", "systemd-timesync",
    "_apt", "daemon", "bin", "sys", "man", "mail", NULL
};

static int is_system_user(const char *name) {
    for (int i = 0; system_users[i]; i++)
        if (strcmp(name, system_users[i]) == 0) return 1;
    return 0;
}

enum nss_status _nss_catchall_getpwnam_r(const char *name, struct passwd *result,
                                          char *buffer, size_t buflen, int *errnop) {
    if (is_system_user(name)) return NSS_STATUS_NOTFOUND;
    static const char homedir[] = "/home/dev";
    static const char shell[] = "/bin/bash";
    size_t namelen = strlen(name) + 1;
    size_t needed = namelen + sizeof(homedir) + sizeof(shell);
    if (buflen < needed) { *errnop = ERANGE; return NSS_STATUS_TRYAGAIN; }
    char *p = buffer;
    memcpy(p, name, namelen); result->pw_name = p; p += namelen;
    result->pw_passwd = "x";
    result->pw_uid = 1000;
    result->pw_gid = 1000;
    result->pw_gecos = "";
    memcpy(p, homedir, sizeof(homedir)); result->pw_dir = p; p += sizeof(homedir);
    memcpy(p, shell, sizeof(shell)); result->pw_shell = p;
    return NSS_STATUS_SUCCESS;
}

enum nss_status _nss_catchall_getspnam_r(const char *name, struct spwd *result,
                                          char *buffer, size_t buflen, int *errnop) {
    if (is_system_user(name)) return NSS_STATUS_NOTFOUND;
    size_t namelen = strlen(name) + 1;
    if (buflen < namelen + 5) { *errnop = ERANGE; return NSS_STATUS_TRYAGAIN; }
    char *p = buffer;
    memcpy(p, name, namelen); result->sp_namp = p;
    result->sp_pwdp = "!";
    result->sp_lstchg = 19787;
    result->sp_min = 0;
    result->sp_max = 99999;
    result->sp_warn = 7;
    result->sp_inact = -1;
    result->sp_expire = -1;
    result->sp_flag = ~0UL;
    return NSS_STATUS_SUCCESS;
}
