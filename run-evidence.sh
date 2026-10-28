#!/bin/bash
W=$HOME/workspace/pr-evidence/pr-710/work
CFG=$W/config.yaml
BB=$HOME/workspace/pr-evidence/pr-710/bin-before/peasant
BA=$HOME/workspace/pr-evidence/pr-710/bin-after/peasant
show(){ # $1=label $2=bin $3=mode $4=dbtag
  echo ""
  echo "### $1"
  db=$W/vdb-$4; rm -rf $db; mkdir -p $db
  if [ "$3" = verbose ]; then
    $2 harvest --data-dir $db --config-dir $db --config $CFG --verbose 2>&1 | grep -E 'ERROR|capture JSONL|session\(s\) failed'
  else
    $2 harvest --data-dir $db --config-dir $db --config $CFG 2>&1 | grep -E 'ERROR|capture JSONL|session\(s\) failed'
  fi
  sleep 2
}
clear
echo "PR #710 - harvest: say why a session failed in the human summary"
echo "Synthetic Claude session, record 2 of 3 is malformed JSONL:"
echo ""
cat -n $W/src/-tmp-qa-reason/dddddddd-0000-4000-8000-000000000001.jsonl | cut -c1-110
sleep 3
show "BEFORE (develop be2bc57f) - default mode" $BB default b1
show "BEFORE (develop be2bc57f) - --verbose" $BB verbose b2
show "AFTER (PR head, rebased a7fffcd6) - default mode" $BA default a1
show "AFTER (PR head, rebased a7fffcd6) - --verbose" $BA verbose a2
echo ""
echo "### Before: only 'ERROR claude-code <id>' - no reason, even --verbose."
echo "### After: the reason prints on an indented line under the row, both modes."
sleep 3
