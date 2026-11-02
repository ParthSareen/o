from storage import get_record, require_owner


def invoice(actor, record_id):
    record = get_record(record_id)
    require_owner(actor, record)
    return record["value"]


def display_label(label):
    return label.strip()
