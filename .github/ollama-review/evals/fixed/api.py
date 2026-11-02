from storage import get_record_for_actor


def invoice(actor, record_id):
    record = get_record_for_actor(actor, record_id)
    return record["value"]
