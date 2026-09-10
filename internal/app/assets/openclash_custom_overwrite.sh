#!/bin/sh

# OpenClash calls this script with the generated YAML path as $1, after its own
# config rewriting. Keep only proxy-group references in the selected group and
# remove individual proxy nodes that were expanded by a trailing `.*` template
# matcher. The operation is idempotent and leaves every other group untouched.

. /usr/share/openclash/ruby.sh 2>/dev/null
. /usr/share/openclash/log.sh 2>/dev/null
. /lib/functions.sh 2>/dev/null

LOG_FILE="/tmp/openclash.log"
CONFIG_FILE="$1"
TARGET_GROUP="${OPENCLASH_CLEAN_GROUP:-🚀 手动选择}"

log_tip() {
    if command -v LOG_TIP >/dev/null 2>&1; then
        LOG_TIP "$1"
    elif command -v LOG_OUT >/dev/null 2>&1; then
        LOG_OUT "$1"
    else
        logger -t openclash-custom-overwrite "$1"
    fi
}

log_warn() {
    if command -v LOG_WARN >/dev/null 2>&1; then
        LOG_WARN "$1"
    else
        logger -t openclash-custom-overwrite "$1"
    fi
}

if [ -z "$CONFIG_FILE" ] || [ ! -s "$CONFIG_FILE" ]; then
    log_warn "Skip group cleanup: generated config is missing: $CONFIG_FILE"
    exit 0
fi

if ! command -v ruby >/dev/null 2>&1; then
    log_warn "Skip group cleanup: Ruby/YAML dependency is not installed"
    exit 0
fi

RESULT="$({ ruby -ryaml -E UTF-8 - "$CONFIG_FILE" "$TARGET_GROUP" <<'RUBY'
config_path, target_name = ARGV
value = YAML.load_file(config_path)
groups = value['proxy-groups']

unless groups.is_a?(Array)
  puts 'missing-groups'
  exit 0
end

target = groups.find do |group|
  group.is_a?(Hash) && group['name'].to_s == target_name
end

unless target
  puts 'missing-target'
  exit 0
end

members = target['proxies']
unless members.is_a?(Array)
  puts 'missing-members'
  exit 0
end

group_names = {}
groups.each do |group|
  next unless group.is_a?(Hash)

  name = group['name'].to_s
  group_names[name] = true unless name.empty?
end

kept = members.each_with_object([]) do |member, result|
  name = member.to_s
  result << member if name != target_name && group_names[name] && !result.include?(member)
end

if kept.empty?
  puts "no-group-members:#{members.length}"
  exit 0
end

if kept == members
  puts "unchanged:#{members.length}"
  exit 0
end

target['proxies'] = kept
mode = File.stat(config_path).mode & 0o777
temp_path = "#{config_path}.custom-overwrite.tmp"
File.open(temp_path, 'w') { |file| file.write(YAML.dump(value)) }
File.chmod(mode, temp_path)
File.rename(temp_path, config_path)
puts "updated:#{members.length}:#{kept.length}"
RUBY
} 2>>"$LOG_FILE")"
STATUS=$?

if [ "$STATUS" -ne 0 ]; then
    log_warn "Group cleanup failed for '$TARGET_GROUP'; original config kept"
    exit 0
fi

case "$RESULT" in
    updated:*)
        BEFORE="$(echo "$RESULT" | cut -d: -f2)"
        AFTER="$(echo "$RESULT" | cut -d: -f3)"
        log_tip "Cleaned '$TARGET_GROUP': kept $AFTER proxy groups, removed $((BEFORE - AFTER)) individual nodes"
        ;;
    unchanged:*)
        log_tip "Group '$TARGET_GROUP' already contains proxy groups only"
        ;;
    missing-target)
        log_warn "Skip group cleanup: '$TARGET_GROUP' does not exist"
        ;;
    no-group-members:*)
        log_warn "Skip group cleanup: '$TARGET_GROUP' has no proxy-group references"
        ;;
    *)
        log_warn "Skip group cleanup: unsupported config structure ($RESULT)"
        ;;
esac

exit 0
