package pgdb

// Housekeeping: nothing a job writes may stay behind and eat disk space.
//
// A job keeps everything under /tmp/sd-<id>* in the containers it uses and restores into a database named
// <target>_sdnew_<id6>, renamed only once complete. A normal end (done, failed or cancelled) removes all of it.
// If the app is killed or loses the network mid-job, the next job on that container finds the leftovers:
// a job is alive while its process runs or its heartbeat file (touched every 2 minutes) is fresh, so jobs
// started from another computer are never touched.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const heartbeatEvery = 2 * time.Minute

// sweepScript removes the leftovers of dead jobs from /tmp. %s is the current job's id.
const sweepScript = `cd /tmp 2>/dev/null || exit 0
seen=" %s "
echo "live %s"
for f in sd-*; do
  [ -e "$f" ] || continue
  id=${f#sd-}; id=${id%%%%.*}
  case "$id" in ''|*[!0-9a-f]*) continue;; esac
  case "$seen" in *" $id "*) continue;; esac
  seen="$seen$id "
  if [ -f "sd-$id.pid" ] && kill -0 "$(cat "sd-$id.pid")" 2>/dev/null; then echo "live $id"; continue; fi
  if [ -n "$(find . -maxdepth 1 -name "sd-$id.alive" -mmin -15 2>/dev/null)" ]; then echo "live $id"; continue; fi
  kb=$(du -sk "sd-$id" sd-$id.* 2>/dev/null | awk '{s+=$1} END {print s+0}')
  rm -rf "sd-$id" sd-$id.*
  echo "removed $id $kb"
done
touch sd-%s.alive`

var tempDBRe = regexp.MustCompile(`_sdnew_([0-9a-f]{6})$`)

// sweepResult is what a sweep found in one container.
type sweepResult struct {
	live    map[string]bool // ids of jobs still running (by 6-char prefix too)
	removed int
	freedKB int64
}

func parseSweep(out string) sweepResult {
	r := sweepResult{live: map[string]bool{}}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 2 && f[0] == "live":
			r.live[f[1]] = true
			if len(f[1]) >= 6 {
				r.live[f[1][:6]] = true
			}
		case len(f) == 3 && f[0] == "removed":
			kb, _ := strconv.ParseInt(f[2], 10, 64)
			r.removed++
			r.freedKB += kb
		}
	}
	return r
}

// housekeep sweeps the containers of this job, drops the temporary databases of dead jobs on the target
// and starts this job's heartbeat.
func (j *Job) housekeep(ctx context.Context) {
	sweep := func(side *Side, container string) sweepResult {
		out, err := run(ctx, side.Host, dexec(side.Docker, container, false, fmt.Sprintf(sweepScript, j.ID, j.ID, j.ID)), time.Minute)
		if err != nil {
			return sweepResult{live: map[string]bool{}}
		}
		r := parseSweep(out)
		if r.removed > 0 {
			j.log(fmt.Sprintf("Removed the temporary files of %d interrupted job(s) on %s (%s freed)", r.removed, side.Server, human(r.freedKB*1024)))
		}
		return r
	}
	live := map[string]bool{}
	if j.spec.hasSource() {
		live = sweep(j.src, j.spec.Source.Container).live
	}
	if !j.spec.hasTarget() {
		return
	}
	if !j.sameContainer() {
		live = sweep(j.dst, j.spec.Target.Container).live
	}
	// Temporary databases of jobs that are gone (none of their connections, no live heartbeat).
	script := pgEnv + "psql -XAtq -d " + quote(j.maintDB()) + " <<'SQL'\n" +
		`select d.datname from pg_database d where d.datname ~ '_sdnew_[0-9a-f]{6}$'
  and not exists (select 1 from pg_stat_activity a where a.application_name like 'sd-' || right(d.datname, 6) || '%');` + "\nSQL\n"
	out, err := run(ctx, j.dst.Host, dexec(j.dst.Docker, j.spec.Target.Container, false, script), time.Minute)
	if err != nil {
		return
	}
	for _, name := range scanLines(out) {
		m := tempDBRe.FindStringSubmatch(name)
		if m == nil || live[m[1]] || m[1] == j.ID[:6] {
			continue
		}
		if err := j.sqlTarget(ctx, dropDatabase(name, j.dstInfo.Version)); err != nil {
			j.warn("Could not drop " + name + ", left by an interrupted job: " + err.Error())
			continue
		}
		j.log("Dropped " + name + ", the unfinished database of an interrupted job, on " + j.dst.Server)
	}
}

// heartbeat tells other jobs that this one is still alive.
func (j *Job) heartbeat() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	touch := func(side *Side, container string) {
		_, _ = run(ctx, side.Host, dexec(side.Docker, container, false, "touch "+j.dir+".alive"), 30*time.Second)
	}
	if j.spec.hasSource() {
		touch(j.src, j.spec.Source.Container)
	}
	if j.spec.hasTarget() && !j.sameContainer() {
		touch(j.dst, j.spec.Target.Container)
	}
}

// removeDump deletes a dump folder as soon as it is not needed any more, to give the space back early.
func (j *Job) removeDump(ctx context.Context, side *Side, container string) {
	if _, err := run(ctx, side.Host, dexec(side.Docker, container, false, "rm -rf "+j.dir), 5*time.Minute); err == nil {
		j.log("Removed the backup files on " + side.Server)
	}
}

func (j *Job) maintDB() string {
	if j.dstInfo.MaintDB != "" {
		return j.dstInfo.MaintDB
	}
	return "postgres"
}
