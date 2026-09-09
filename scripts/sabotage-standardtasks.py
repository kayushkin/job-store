#!/usr/bin/env python3
"""Score whether anything notices the shipped standard-application-task tables changing.

The mechanism under test is the CONTENT of the two tables in noteboard.go that
decide what lands in the user's todo queue for every new application:
StandardApplicationTasks (three title formats plus three due offsets) and
StandardApplicationTaskTags. A row, a wording, an offset or a tag that changes
without a test going red is a follow-up that silently stops being created, or is
created saying something else.

Each case rewrites one or more files, runs the package suite, and restores them.
CAUGHT means the suite went red, and the row records which tests fired. SILENT
means it stayed green and nothing on this box would notice that edit.

The last group of cases edits the TEST LITERAL alongside the shipped table, which
is what an author adding a fourth row would do. Those rows are the only evidence
that the two structural tests carry weight: every other defect is caught by the
literal comparison, which would shadow a structural test that never worked.

Run from the repo root. The suite needs -tags sqlite_fts5: without it job-store
is red from a clean tree ("no such module: fts5") and every row would read CAUGHT
against a baseline that was never green.
"""

import pathlib
import re
import subprocess
import sys

import os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tree_hold  # noqa: E402  vendored; see tree_hold.py on keeping copies identical

REPO = pathlib.Path(__file__).resolve().parent.parent
SOURCE = "noteboard.go"
TESTS = "standardtasks_test.go"
TEST_COMMAND = ["go", "test", "-tags", "sqlite_fts5", "./..."]
PACKAGE_PASSED = "ok  \tgithub.com/kayushkin/job-store\t"

# The fourth row each "added row" case appends, in the shipped table and in the
# test's literal. Only the malformed part differs, so what the structural tests
# catch is isolated to that.
ROW_ANCHOR = '\t{TitleFormat: "Record the outcome of the %s %s application, or mark it ghosted", DueInDays: 30},\n'
WANT_ANCHOR = '\t\t{TitleFormat: "Record the outcome of the %s %s application, or mark it ghosted", DueInDays: 30},\n'


def added_row(source_row, want_row):
    """Edits appending one row to the shipped table and the same row to the literal."""
    return [
        (SOURCE, ROW_ANCHOR, ROW_ANCHOR + "\t" + source_row + "\n"),
        (TESTS, WANT_ANCHOR, WANT_ANCHOR + "\t\t" + want_row + "\n"),
    ]


# Each case is (name, [(file, old, new), ...]). Every `old` must appear exactly
# once in its file: an anchor matching nothing reads as SKIPPED, and an anchor
# matching the wrong occurrence reads as a passing control, which is the silent
# half of the same failure.
CASES = [
    (
        "a shipped row is deleted (the interview-prep follow-up never gets created)",
        [(SOURCE, '\t{TitleFormat: "Research %s and prepare questions for the %s interview", DueInDays: 3},\n', "")],
    ),
    (
        "a title format is reworded (the todo says something the product never agreed)",
        [(SOURCE, '"Follow up with %s about the %s application if no reply in 7 days"', '"Ping %s re: %s"')],
    ),
    (
        "a title format swaps company and role (every todo reads backwards)",
        [(
            SOURCE,
            '"Record the outcome of the %s %s application, or mark it ghosted"',
            '"Record the outcome of the %s application at %s, or mark it ghosted"',
        )],
    ),
    (
        "a due offset drifts (the 7-day follow-up comes due in 70)",
        [(
            SOURCE,
            '"Follow up with %s about the %s application if no reply in 7 days", DueInDays: 7',
            '"Follow up with %s about the %s application if no reply in 7 days", DueInDays: 70',
        )],
    ),
    (
        "a due offset goes to zero (the todo is due the moment it is created)",
        [(
            SOURCE,
            '"Research %s and prepare questions for the %s interview", DueInDays: 3',
            '"Research %s and prepare questions for the %s interview", DueInDays: 0',
        )],
    ),
    (
        "the personal tag is dropped (the follow-ups land in the coding queue)",
        [(SOURCE, 'var StandardApplicationTaskTags = []string{"jobs", "personal"}',
          'var StandardApplicationTaskTags = []string{"jobs"}')],
    ),
    (
        "the tag set is replaced wholesale (no reminder surface claims them)",
        [(SOURCE, 'var StandardApplicationTaskTags = []string{"jobs", "personal"}',
          'var StandardApplicationTaskTags = []string{"whatever"}')],
    ),
    # These three are the reason the structural tests exist. The author updated
    # the literal, so TestStandardApplicationTasksShipExactlyTheseThreeFollowUps
    # is satisfied and only a structural rule can object.
    (
        "a fourth row is added taking ONE verb (the title carries an fmt error)",
        added_row(
            '{TitleFormat: "Chase %s", DueInDays: 14},',
            '{TitleFormat: "Chase %s", DueInDays: 14},',
        ),
    ),
    # Indexed verbs are the only way a format takes the two arguments in the
    # other order. An earlier version of this case used "Prepare the %s pitch
    # for %s" and SURVIVED — correctly, because two plain verbs are filled
    # company-first whatever the surrounding English, so that row never had the
    # defect its name claimed. A mutation that does not produce its own defect
    # reads as a gap in the tests and is really a gap in the case list.
    (
        "a fourth row is added with indexed verbs, taking role then company",
        added_row(
            '{TitleFormat: "Interview prep for the %[2]s role at %[1]s", DueInDays: 5},',
            '{TitleFormat: "Interview prep for the %[2]s role at %[1]s", DueInDays: 5},',
        ),
    ),
    (
        "a fourth row is added due on day zero (overdue the instant it exists)",
        added_row(
            '{TitleFormat: "Send %s the %s take-home", DueInDays: 0},',
            '{TitleFormat: "Send %s the %s take-home", DueInDays: 0},',
        ),
    ),
    # A control. It rewords a doc comment, which no assertion can reach. A suite
    # reporting this CAUGHT is reporting noise and every row above is void.
    # Proving the instrument can say "no" before trusting it saying "yes".
    (
        "CONTROL: a doc comment is reworded, which nothing can observe",
        [(
            SOURCE,
            "// StandardApplicationTaskTags is what every standard todo is tagged with, so the",
            "// StandardApplicationTaskTags lists the tags put on every standard todo, so the",
        )],
    ),
]


def run_suite():
    """Return (green, failing test names, raw output).

    Reads the return code and requires the package's own `ok` line. Grepping for
    FAIL is not enough: a mutation that stops the package compiling produces no
    test output at all, which reads exactly like a clean pass.
    """
    done = subprocess.run(TEST_COMMAND, cwd=REPO, capture_output=True, text=True)
    output = done.stdout + done.stderr
    passed = PACKAGE_PASSED in output
    failing = sorted(set(re.findall(r"^--- FAIL: (\w+)", output, re.M)))
    return (done.returncode == 0 and passed), failing, output


def main():
    """Take this tree exclusively, then run. Call this, not `main_on_a_held_tree`.

    Card `d869d2be`. Every verdict below is read off the SUITE'S exit code, and that
    exit code belongs to the whole tree rather than to the mutation this run wrote. A
    second run mutating the same tree hands this one a red suite it did not cause, and
    this one records it as CAUGHT — the collision does not add noise, it **inflates the
    score**, and these scores are the numbers the nightly write-ups quote.

    It refuses rather than waits. A run told to come back later can say so and exit;
    one silently blocked for the length of somebody else's suite looks hung. The
    refusal exits non-zero, because a caller reading exit 0 would read "measured, and
    clean" from a run that measured nothing.

    The restore-on-signal handling below is a different guard. It stops THIS run
    leaving a mutation behind. It cannot see a concurrent run at all, because the
    other run restores each file before its next case and the tree is clean between
    mutations exactly when it is most dangerous to trust.
    """
    with tree_hold.exclusive_hold_on_tree(
            REPO, purpose=os.path.basename(sys.argv[0] or "sabotage-standardtasks")) as refusal:
        if refusal:
            sys.exit("REFUSING: " + refusal)
        return main_on_a_held_tree()


def main_on_a_held_tree():
    paths = {name: REPO / name for name in (SOURCE, TESTS)}
    originals = {name: path.read_text() for name, path in paths.items()}

    green, _, output = run_suite()
    if not green:
        print("BASELINE IS RED — every row below would read CAUGHT against it. Stopping.")
        print(output[-2000:])
        return 1
    print("baseline: green\n")

    results = []
    for name, edits in CASES:
        bad_anchor = None
        for filename, old, _ in edits:
            occurrences = originals[filename].count(old)
            if occurrences != 1:
                bad_anchor = f"SKIPPED ({filename} anchor matched {occurrences} times, not 1)"
                break
        if bad_anchor:
            results.append((name, bad_anchor, []))
            print(f"{bad_anchor}  {name}")
            continue

        mutated = dict(originals)
        for filename, old, new in edits:
            mutated[filename] = mutated[filename].replace(old, new, 1)
        try:
            for filename, text in mutated.items():
                paths[filename].write_text(text)
            green, failing, output = run_suite()
        finally:
            for filename, text in originals.items():
                paths[filename].write_text(text)

        if green:
            verdict = "SILENT"
        elif not failing and PACKAGE_PASSED not in output:
            verdict = "VOID (did not compile — not a survivor and not a catch)"
        else:
            verdict = "CAUGHT"
        results.append((name, verdict, failing))
        detectors = ", ".join(failing) if failing else "-"
        print(f"{verdict:8} {name}\n         detected by: {detectors}")

    print()
    scored = [r for r in results if r[1] in ("CAUGHT", "SILENT")]
    caught = [r for r in scored if r[1] == "CAUGHT"]
    print(f"{len(caught)}/{len(scored)} caught")
    for name, verdict, _ in results:
        if verdict.startswith(("SKIPPED", "VOID")):
            print(f"  !! {verdict}: {name}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
