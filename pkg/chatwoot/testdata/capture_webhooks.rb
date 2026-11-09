# Used by live_test.go. Builds webhooks with Chatwoot's own code and captures
# the signed HTTP requests instead of sending them.
#
#   bundle exec rails runner capture_webhooks.rb requests.json webhooks.json
#
# Each request is built like WebhookListener does (webhook_data + event name)
# and passed to Webhooks::Trigger, which serializes and signs it. SafeFetch, the
# network call, is replaced by a capture.

require 'json'

requests_path, output_path = ARGV
requests = JSON.parse(File.read(requests_path))
captured = []

capture = Module.new do
  define_method(:fetch) do |url, **options, &_block|
    captured << { url: url, headers: options[:headers], body: options[:body] }
    nil
  end
end
SafeFetch.singleton_class.prepend(capture)

results = requests.map do |req|
  captured.clear
  if req['message_id']
    message = Message.find(req['message_id'])
    payload = message.webhook_data.merge(event: req['event'])
    inbox = message.inbox
  else
    conversation = Conversation.find_by!(display_id: req['conversation_id'], account_id: Account.order(:id).first.id)
    payload = conversation.webhook_data.merge(event: req['event'], changed_attributes: [])
    inbox = conversation.inbox
  end
  # The test inbox has no webhook URL (so Chatwoot does not try to deliver);
  # the test passes the URL the integration configured.
  Webhooks::Trigger.execute(req['url'], payload, :api_inbox_webhook,
                            secret: inbox.channel.secret, delivery_id: SecureRandom.uuid)
  raise "nothing captured for #{req['label']}" if captured.empty?

  captured.first.merge(label: req['label'])
end

File.write(output_path, JSON.generate(results))
puts "captured #{results.size} webhooks"
