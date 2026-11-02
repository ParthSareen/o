from storage import get_record, require_owner


def invoice(actor, record_id):
    record = get_record(record_id)
    return record["value"]
