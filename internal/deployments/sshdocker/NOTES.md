# sshdocker notes

Lore for `internal/deployments/sshdocker`, the fallback executor that runs an
engine container over SSH for providers that rent machines rather than start
images. Lambda Labs is the only such provider today; raw AWS and GCP land here
too (#428). The image-native providers (RunPod, Vast since #252) never touch
this package.

## The SSH user is not always root, and docker is the first thing that notices

Every provider this executor was written against logs in as root, so the
question never came up. Lambda logs in as `ubuntu`. Lambda's image ships
Docker 28.3.1 and puts that user in `ubuntu, users, admin`, and **not** in
`docker`, so the socket is unreachable:

```
uid=1000(ubuntu) gid=1000(ubuntu) groups=1000(ubuntu),100(users),118(admin)
docker ps        -> permission denied ... unix:///var/run/docker.sock
sudo -n docker ps -> works
```

Every command in the deploy path failed at the first `docker inspect`. Found
on a rented A10 on the first live Lambda deploy anybody had ever driven
(#427); it is not visible from any unit test, because the fake runner answers
whatever it is asked.

`resolveElevation` probes `docker info`, then `sudo -n docker info`, and
prefixes commands with `sudo ` when the second is what works. **Probing beats
inferring from the username.** Group membership, socket permissions and
rootless docker can each make the answer differ from what `id` suggests, and
`docker info` is the question actually being asked. `sudo -n` rather than
`sudo`, so a host that would prompt fails immediately instead of hanging on a
tty that is not there.

**The probe is lazy, not part of `EnsureInstalled`.** That was the first
attempt and the live teardown failed the same way an hour later: `Destroy`
builds its own `Docker` and never calls `EnsureInstalled`, so anything
resolved only there is missing on every teardown. Resolving on first use, once
per `Docker`, means a path that forgets a setup step cannot be wrong. If you
add a command here, route it through `run` or `dockerCmd` and it inherits
this.

When neither route works the error names both attempts and the fix
(`usermod -aG docker`), because nothing else on the box explains why an
installed docker is unusable.
